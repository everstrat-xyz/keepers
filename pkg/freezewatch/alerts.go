// Package freezewatch evaluates the freeze precursors and keeper-health
// signals W4 alerts on.
//
// Everything here is pure: an Observation in, Alerts out. The workflow does the
// reading and the notifying, so the thresholds — the part that gets argued
// about and tuned — are testable without a chain.
//
// # No writes, structurally
//
// W4 is observability only. This package has no dependency on pkg/crewrite or
// pkg/envelope, and there is no code path from an Alert to a report. That is
// the guarantee, not a convention: a future edit that tried to actuate would
// have to add the import, which is visible in review.
package freezewatch

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/everstrat-xyz/keepers/pkg/registry"
)

// Severity ranks an alert for routing.
type Severity string

const (
	// SeverityWarning — a precursor. Someone should look today.
	SeverityWarning Severity = "warning"
	// SeverityCritical — user funds or protocol liveness are at risk now.
	SeverityCritical Severity = "critical"
)

// Kind identifies what fired. Stable strings: alert routing and dashboards key
// off them.
type Kind string

const (
	KindOracleStale         Kind = "oracle-stale"
	KindBatchEscapeHatch    Kind = "batch-escape-hatch"
	KindProtocolPaused      Kind = "protocol-paused"
	KindReceiverUnbound     Kind = "receiver-unbound"
	KindKeeperStalled       Kind = "keeper-stalled"
	KindStrategyUnhealthy   Kind = "strategy-unhealthy"
	KindUpkeepBacklog       Kind = "upkeep-backlog"
)

// Alert is one firing condition.
type Alert struct {
	Kind     Kind
	Severity Severity
	Subject  string // the address or id the alert is about, when there is one
	Message  string
}

func (a Alert) String() string {
	if a.Subject == "" {
		return fmt.Sprintf("[%s] %s: %s", a.Severity, a.Kind, a.Message)
	}
	return fmt.Sprintf("[%s] %s (%s): %s", a.Severity, a.Kind, a.Subject, a.Message)
}

// Thresholds are the tuning knobs, all in seconds unless noted.
//
// Defaults are deliberately conservative: W4 exists to give warning, so it
// fires while there is still time to act rather than at the moment of failure.
type Thresholds struct {
	// OracleStaleAfter is how old a price may be before warning. It should sit
	// below the staleness bound the contracts themselves enforce, so the alert
	// precedes the revert.
	OracleStaleAfter uint64
	// EscapeHatchWarnWithin fires when a priced, unprocessed batch is within
	// this long of MAX_BATCH_PROCESSING_TIME, after which users must close
	// their own requests.
	EscapeHatchWarnWithin uint64
	// KeeperStalledAfter fires when a receiver has accepted no report for this
	// long while upkeep was available — the silent-stop failure mode.
	KeeperStalledAfter uint64
	// BacklogWarnBatches fires when this many batches are waiting behind the
	// cursor.
	BacklogWarnBatches uint64
}

// DefaultThresholds are the starting values. Tune against FREEZE_RUNBOOK.md.
func DefaultThresholds() Thresholds {
	return Thresholds{
		OracleStaleAfter:      3 * 3600,
		EscapeHatchWarnWithin: 12 * 3600,
		KeeperStalledAfter:    6 * 3600,
		BacklogWarnBatches:    10,
	}
}

// OracleFeed is one price feed's freshness.
type OracleFeed struct {
	Pair      string
	UpdatedAt uint64
	Price     *big.Int
}

// QueueBatch is a batch's escape-hatch exposure.
type QueueBatch struct {
	ID               uint64
	PricedAt         uint64
	UnprocessedCount uint64
}

// StrategyHealth is one strategy's status.
type StrategyHealth struct {
	Address string
	Paused  bool
	Healthy bool
}

// Observation is everything W4 read this tick.
type Observation struct {
	Now uint64

	// Protocol is the address book this observation was read through, so an
	// alert can be traced to the deployment it came from.
	Protocol registry.Protocol

	ControllerPaused      bool
	ExitQueuePaused       bool
	AMMPaused             bool
	StrategyManagerPaused bool

	MaxBatchProcessingTime uint64
	Batches                []QueueBatch
	// BacklogBatches is how many batches sit between the queue cursor and the
	// current batch.
	BacklogBatches uint64

	Feeds      []OracleFeed
	Strategies []StrategyHealth

	// Keepers is per-receiver liveness.
	Keepers []KeeperHealth
}

// KeeperHealth describes one CRE receiver.
type KeeperHealth struct {
	Name string
	// Bound is false when neither expectedWorkflowId nor expectedAuthor is
	// set, which makes the receiver inert — it rejects every report.
	Bound bool
	// LastAcceptedAt is when the receiver last accepted a report. Zero means
	// never, which is expected before cutover.
	//
	// CONTRACT-BLOCKED: no receiver view exposes this — `readKeepers` in
	// freeze-watch/reads.go cannot populate it, so KindKeeperStalled never
	// fires in production today, and the unit tests (which set this field
	// directly) mask that. Needs a `lastReportAcceptedAt()` accessor (or
	// equivalent) on CREReceiverBase in everstrat-xyz/contracts; wire it up
	// in readKeepers as part of that change.
	LastAcceptedAt uint64
	// UpkeepAvailable is whether the receiver's own view currently recommends
	// an action. A keeper is only "stalled" if there was work to do.
	UpkeepAvailable bool
	Paused          bool
}

// Evaluate returns every firing alert, most severe first.
//
// It never returns an error: a monitor that can fail to evaluate is a monitor
// that goes quiet exactly when things are going wrong. Missing inputs simply
// produce no alert for that check, and the workflow logs what it could not read.
func Evaluate(o Observation, t Thresholds) []Alert {
	var alerts []Alert

	// Pause states. Not a precursor — the protocol is already halted.
	for _, p := range []struct {
		name   string
		paused bool
	}{
		{"Controller", o.ControllerPaused},
		{"ExitQueue", o.ExitQueuePaused},
		{"AMM", o.AMMPaused},
		{"StrategyManager", o.StrategyManagerPaused},
	} {
		if p.paused {
			alerts = append(alerts, Alert{
				Kind:     KindProtocolPaused,
				Severity: SeverityCritical,
				Subject:  p.name,
				Message:  fmt.Sprintf("%s is paused; keeper actions revert until it is unpaused", p.name),
			})
		}
	}

	// Oracle staleness. A stale feed is what tips NAV into freeze mode.
	for _, f := range o.Feeds {
		if f.UpdatedAt == 0 || o.Now < f.UpdatedAt {
			continue
		}
		if age := o.Now - f.UpdatedAt; age > t.OracleStaleAfter {
			alerts = append(alerts, Alert{
				Kind:     KindOracleStale,
				Severity: SeverityCritical,
				Subject:  f.Pair,
				Message: fmt.Sprintf("price is %ds old (threshold %ds); NAV reads will revert once the contract's own staleness bound is crossed",
					age, t.OracleStaleAfter),
			})
		}
	}

	// Batches approaching the escape hatch. After it, users must close their
	// own requests and the keeper can no longer settle them.
	for _, b := range o.Batches {
		if b.PricedAt == 0 || b.UnprocessedCount == 0 || o.MaxBatchProcessingTime == 0 {
			continue
		}
		deadline := b.PricedAt + o.MaxBatchProcessingTime
		if o.Now >= deadline {
			alerts = append(alerts, Alert{
				Kind:     KindBatchEscapeHatch,
				Severity: SeverityCritical,
				Subject:  fmt.Sprintf("batch %d", b.ID),
				Message: fmt.Sprintf("passed MAX_BATCH_PROCESSING_TIME with %d requests unprocessed; users must now close their own",
					b.UnprocessedCount),
			})
			continue
		}
		if remaining := deadline - o.Now; remaining <= t.EscapeHatchWarnWithin {
			alerts = append(alerts, Alert{
				Kind:     KindBatchEscapeHatch,
				Severity: SeverityWarning,
				Subject:  fmt.Sprintf("batch %d", b.ID),
				Message: fmt.Sprintf("%ds from the escape hatch with %d requests unprocessed",
					remaining, b.UnprocessedCount),
			})
		}
	}

	if t.BacklogWarnBatches > 0 && o.BacklogBatches >= t.BacklogWarnBatches {
		alerts = append(alerts, Alert{
			Kind:     KindUpkeepBacklog,
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("%d batches are waiting behind the queue cursor", o.BacklogBatches),
		})
	}

	// Keeper liveness — the silent-stop failure mode W4 exists for.
	for _, k := range o.Keepers {
		switch {
		case !k.Bound:
			alerts = append(alerts, Alert{
				Kind:     KindReceiverUnbound,
				Severity: SeverityWarning,
				Subject:  k.Name,
				Message:  "receiver has neither expectedWorkflowId nor expectedAuthor set, so it rejects every report",
			})
		case k.Paused:
			alerts = append(alerts, Alert{
				Kind:     KindProtocolPaused,
				Severity: SeverityCritical,
				Subject:  k.Name,
				Message:  "receiver is paused",
			})
		case k.UpkeepAvailable && k.LastAcceptedAt > 0 && o.Now >= k.LastAcceptedAt:
			if idle := o.Now - k.LastAcceptedAt; idle > t.KeeperStalledAfter {
				alerts = append(alerts, Alert{
					Kind:     KindKeeperStalled,
					Severity: SeverityCritical,
					Subject:  k.Name,
					Message: fmt.Sprintf("upkeep has been available but no report has been accepted for %ds (threshold %ds)",
						idle, t.KeeperStalledAfter),
				})
			}
		}
	}

	// Unhealthy strategies. A paused one is already known to operators.
	for _, s := range o.Strategies {
		if !s.Paused && !s.Healthy {
			alerts = append(alerts, Alert{
				Kind:     KindStrategyUnhealthy,
				Severity: SeverityWarning,
				Subject:  s.Address,
				Message:  "strategy reports unhealthy and is not paused; a rebalance is due",
			})
		}
	}

	sortAlerts(alerts)
	return alerts
}

// sortAlerts puts critical first, then groups by kind, so a digest reads in
// priority order rather than in read order.
func sortAlerts(alerts []Alert) {
	rank := map[Severity]int{SeverityCritical: 0, SeverityWarning: 1}
	sort.SliceStable(alerts, func(i, j int) bool {
		if rank[alerts[i].Severity] != rank[alerts[j].Severity] {
			return rank[alerts[i].Severity] < rank[alerts[j].Severity]
		}
		return alerts[i].Kind < alerts[j].Kind
	})
}

// Summary counts alerts by severity, for the tick log and the notification
// subject line.
type Summary struct {
	Critical int
	Warning  int
}

// Summarize counts an alert slice.
func Summarize(alerts []Alert) Summary {
	var s Summary
	for _, a := range alerts {
		switch a.Severity {
		case SeverityCritical:
			s.Critical++
		case SeverityWarning:
			s.Warning++
		}
	}
	return s
}

// Subject renders a one-line notification subject.
func (s Summary) Subject() string {
	switch {
	case s.Critical > 0:
		return fmt.Sprintf("EverStrat: %d critical, %d warning", s.Critical, s.Warning)
	case s.Warning > 0:
		return fmt.Sprintf("EverStrat: %d warning", s.Warning)
	default:
		return "EverStrat: all clear"
	}
}

// Empty reports whether nothing fired.
func (s Summary) Empty() bool { return s.Critical == 0 && s.Warning == 0 }
