package freezewatch_test

import (
	"strings"
	"testing"

	"github.com/everstrat-xyz/keepers/pkg/freezewatch"
)

const now = uint64(1_700_000_000)

func healthy() freezewatch.Observation {
	return freezewatch.Observation{
		Now:                    now,
		MaxBatchProcessingTime: 3 * 86400,
		Feeds: []freezewatch.OracleFeed{
			{Pair: "ETH/USD", UpdatedAt: now - 60},
		},
		Strategies: []freezewatch.StrategyHealth{
			{Address: "0xstrat", Healthy: true},
		},
		Keepers: []freezewatch.KeeperHealth{
			{Name: "queue-keeper", Bound: true, LastAcceptedAt: now - 60},
			{Name: "strategy-keeper", Bound: true, LastAcceptedAt: now - 60},
		},
	}
}

func kinds(alerts []freezewatch.Alert) map[freezewatch.Kind]freezewatch.Alert {
	out := map[freezewatch.Kind]freezewatch.Alert{}
	for _, a := range alerts {
		out[a.Kind] = a
	}
	return out
}

func TestHealthyProtocolIsQuiet(t *testing.T) {
	alerts := freezewatch.Evaluate(healthy(), freezewatch.DefaultThresholds())
	if len(alerts) != 0 {
		t.Errorf("a healthy protocol produced %d alerts: %v", len(alerts), alerts)
	}
	if !freezewatch.Summarize(alerts).Empty() {
		t.Error("Summary.Empty() = false for no alerts")
	}
}

func TestOracleStaleness(t *testing.T) {
	th := freezewatch.DefaultThresholds()

	t.Run("fresh feed is quiet", func(t *testing.T) {
		o := healthy()
		o.Feeds[0].UpdatedAt = now - th.OracleStaleAfter // exactly at the bound
		if got := kinds(freezewatch.Evaluate(o, th)); len(got) != 0 {
			t.Errorf("a feed exactly at the threshold alerted: %v", got)
		}
	})

	t.Run("stale feed is critical", func(t *testing.T) {
		o := healthy()
		o.Feeds[0].UpdatedAt = now - th.OracleStaleAfter - 1
		got := kinds(freezewatch.Evaluate(o, th))
		a, ok := got[freezewatch.KindOracleStale]
		if !ok {
			t.Fatalf("no oracle-stale alert: %v", got)
		}
		if a.Severity != freezewatch.SeverityCritical {
			t.Errorf("severity = %s, want critical", a.Severity)
		}
		if a.Subject != "ETH/USD" {
			t.Errorf("subject = %q, want the pair", a.Subject)
		}
	})

	t.Run("unread feed does not alert", func(t *testing.T) {
		// A zero timestamp means the read failed, not that the feed is
		// infinitely old — alerting would be a false positive.
		o := healthy()
		o.Feeds[0].UpdatedAt = 0
		if got := kinds(freezewatch.Evaluate(o, th)); len(got) != 0 {
			t.Errorf("an unread feed alerted: %v", got)
		}
	})

	t.Run("future-dated feed does not alert", func(t *testing.T) {
		o := healthy()
		o.Feeds[0].UpdatedAt = now + 100
		if got := kinds(freezewatch.Evaluate(o, th)); len(got) != 0 {
			t.Errorf("a future-dated feed alerted (underflow?): %v", got)
		}
	})
}

func TestBatchEscapeHatch(t *testing.T) {
	th := freezewatch.DefaultThresholds()
	maxProc := uint64(3 * 86400)

	tests := []struct {
		name         string
		pricedAt     uint64
		unprocessed  uint64
		wantAlert    bool
		wantSeverity freezewatch.Severity
	}{
		{
			name:        "freshly priced batch is quiet",
			pricedAt:    now - 3600,
			unprocessed: 5,
		},
		{
			name:         "batch inside the warning window",
			pricedAt:     now - maxProc + th.EscapeHatchWarnWithin - 1,
			unprocessed:  5,
			wantAlert:    true,
			wantSeverity: freezewatch.SeverityWarning,
		},
		{
			name:         "batch past the escape hatch is critical",
			pricedAt:     now - maxProc - 1,
			unprocessed:  5,
			wantAlert:    true,
			wantSeverity: freezewatch.SeverityCritical,
		},
		{
			name:        "fully processed batch is quiet even when overdue",
			pricedAt:    now - maxProc - 1,
			unprocessed: 0,
		},
		{
			name:        "unpriced batch is quiet",
			pricedAt:    0,
			unprocessed: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := healthy()
			o.Batches = []freezewatch.QueueBatch{
				{ID: 7, PricedAt: tt.pricedAt, UnprocessedCount: tt.unprocessed},
			}
			got := kinds(freezewatch.Evaluate(o, th))
			a, ok := got[freezewatch.KindBatchEscapeHatch]
			if ok != tt.wantAlert {
				t.Fatalf("alert present = %v, want %v (%v)", ok, tt.wantAlert, got)
			}
			if tt.wantAlert && a.Severity != tt.wantSeverity {
				t.Errorf("severity = %s, want %s", a.Severity, tt.wantSeverity)
			}
		})
	}
}

func TestKeeperHealth(t *testing.T) {
	th := freezewatch.DefaultThresholds()

	t.Run("unbound receiver warns", func(t *testing.T) {
		o := healthy()
		o.Keepers[0].Bound = false
		a, ok := kinds(freezewatch.Evaluate(o, th))[freezewatch.KindReceiverUnbound]
		if !ok {
			t.Fatal("no receiver-unbound alert")
		}
		if !strings.Contains(a.Message, "rejects every report") {
			t.Errorf("message does not explain the consequence: %q", a.Message)
		}
	})

	t.Run("stalled keeper with available upkeep is critical", func(t *testing.T) {
		o := healthy()
		o.Keepers[0].UpkeepAvailable = true
		o.Keepers[0].LastAcceptedAt = now - th.KeeperStalledAfter - 1
		a, ok := kinds(freezewatch.Evaluate(o, th))[freezewatch.KindKeeperStalled]
		if !ok {
			t.Fatal("no keeper-stalled alert")
		}
		if a.Severity != freezewatch.SeverityCritical {
			t.Errorf("severity = %s, want critical", a.Severity)
		}
	})

	t.Run("idle keeper with no upkeep available is quiet", func(t *testing.T) {
		// The distinction that keeps W4 from crying wolf: a keeper that has
		// not acted because there was nothing to do is working correctly.
		o := healthy()
		o.Keepers[0].UpkeepAvailable = false
		o.Keepers[0].LastAcceptedAt = now - 30*86400
		if got := kinds(freezewatch.Evaluate(o, th)); len(got) != 0 {
			t.Errorf("an idle keeper with no work alerted: %v", got)
		}
	})

	t.Run("never-accepted keeper does not alert as stalled", func(t *testing.T) {
		// Expected before cutover; LastAcceptedAt is zero, not ancient.
		o := healthy()
		o.Keepers[0].UpkeepAvailable = true
		o.Keepers[0].LastAcceptedAt = 0
		if _, ok := kinds(freezewatch.Evaluate(o, th))[freezewatch.KindKeeperStalled]; ok {
			t.Error("a keeper that has never accepted a report alerted as stalled")
		}
	})
}

func TestPausedContracts(t *testing.T) {
	o := healthy()
	o.ControllerPaused = true
	o.AMMPaused = true

	alerts := freezewatch.Evaluate(o, freezewatch.DefaultThresholds())
	var paused int
	for _, a := range alerts {
		if a.Kind == freezewatch.KindProtocolPaused {
			paused++
			if a.Severity != freezewatch.SeverityCritical {
				t.Errorf("%s severity = %s, want critical", a.Subject, a.Severity)
			}
		}
	}
	if paused != 2 {
		t.Errorf("got %d paused alerts, want 2", paused)
	}
}

func TestStrategySignals(t *testing.T) {
	o := healthy()
	o.Strategies = []freezewatch.StrategyHealth{
		{Address: "0xa", Healthy: false},
		{Address: "0xb", Healthy: false, Paused: true}, // already known to ops
		{Address: "0xc", Healthy: true},
	}

	alerts := freezewatch.Evaluate(o, freezewatch.DefaultThresholds())

	var unhealthy int
	for _, a := range alerts {
		if a.Kind == freezewatch.KindStrategyUnhealthy {
			unhealthy++
		}
	}
	if unhealthy != 1 {
		t.Errorf("got %d unhealthy alerts, want 1 (a paused strategy is already known)", unhealthy)
	}
}

func TestBacklog(t *testing.T) {
	th := freezewatch.DefaultThresholds()
	o := healthy()
	o.BacklogBatches = th.BacklogWarnBatches

	if _, ok := kinds(freezewatch.Evaluate(o, th))[freezewatch.KindUpkeepBacklog]; !ok {
		t.Error("no backlog alert at the threshold")
	}

	o.BacklogBatches = th.BacklogWarnBatches - 1
	if _, ok := kinds(freezewatch.Evaluate(o, th))[freezewatch.KindUpkeepBacklog]; ok {
		t.Error("backlog alerted below the threshold")
	}
}

func TestCriticalAlertsSortFirst(t *testing.T) {
	o := healthy()
	o.Strategies[0].Healthy = false        // warning
	o.ControllerPaused = true              // critical
	o.Feeds[0].UpdatedAt = now - 100*86400 // critical

	alerts := freezewatch.Evaluate(o, freezewatch.DefaultThresholds())
	if len(alerts) < 3 {
		t.Fatalf("got %d alerts, want at least 3", len(alerts))
	}

	seenWarning := false
	for _, a := range alerts {
		if a.Severity == freezewatch.SeverityWarning {
			seenWarning = true
			continue
		}
		if seenWarning {
			t.Errorf("critical alert %s sorted after a warning", a.Kind)
		}
	}

	s := freezewatch.Summarize(alerts)
	if s.Critical != 2 || s.Warning != 1 {
		t.Errorf("summary = %d critical / %d warning, want 2/1", s.Critical, s.Warning)
	}
	if !strings.Contains(s.Subject(), "2 critical") {
		t.Errorf("subject = %q, want it to lead with the critical count", s.Subject())
	}
}

func TestSummarySubject(t *testing.T) {
	if got := freezewatch.Summarize(nil).Subject(); got != "EverStrat: all clear" {
		t.Errorf("empty subject = %q", got)
	}
	warnOnly := []freezewatch.Alert{{Severity: freezewatch.SeverityWarning}}
	if got := freezewatch.Summarize(warnOnly).Subject(); !strings.Contains(got, "1 warning") {
		t.Errorf("warning subject = %q", got)
	}
}
