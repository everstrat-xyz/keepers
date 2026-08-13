//go:build wasip1

package main

import (
	"fmt"
	"log/slog"

	"github.com/smartcontractkit/cre-sdk-go/capabilities/scheduler/cron"
	"github.com/smartcontractkit/cre-sdk-go/cre"
	"github.com/smartcontractkit/cre-sdk-go/cre/wasm"

	"github.com/everstrat-xyz/keepers/pkg/chains"
	"github.com/everstrat-xyz/keepers/pkg/envelope"
	"github.com/everstrat-xyz/keepers/pkg/evmread"
	"github.com/everstrat-xyz/keepers/pkg/queue"
)

// defaultMaxBatchScan bounds how many batches one tick reads.
//
// It is an order of magnitude past the on-chain view's 25-batch window — which
// is where W1's advantage comes from — while still refusing to issue an
// unbounded number of calls after a long stall.
const defaultMaxBatchScan = 250

// Config is loaded from config.<target>.json (see workflow.yaml).
type Config struct {
	chains.Config
	Schedule             string `json:"schedule"`
	QueueExecutorAddress string `json:"queueExecutorAddress"`

	// MaxBatchScan caps the off-chain scan width. 0 uses defaultMaxBatchScan.
	MaxBatchScan uint64 `json:"maxBatchScan"`

	// BlockTag selects which block reads observe. Defaults to "finalized",
	// which is the only setting safe for DON consensus; "latest" exists for
	// local simulation against a single node.
	BlockTag string `json:"blockTag"`
}

// Result is the tick's structured outcome. Shadow-mode monitoring (issue #5)
// reads these fields, so they are part of the interface, not debug output.
type Result struct {
	Bound        bool   `json:"bound"`
	ShadowMode   bool   `json:"shadowMode"`
	Action       string `json:"action"`
	BatchID      uint64 `json:"batchId"`
	EndIndex     uint64 `json:"endIndex"`
	Divergence   string `json:"divergence"`
	Wrote        bool   `json:"wrote"`
	TxHash       string `json:"txHash,omitempty"`
	ScanTruncate bool   `json:"scanTruncated"`
	Message      string `json:"message"`
}

func onCronTrigger(config *Config, runtime cre.Runtime, _ *cron.Payload) (*Result, error) {
	logger := runtime.Logger()

	deployment, err := chains.Resolve(config.Config, config.QueueExecutorAddress)
	if err != nil {
		// Before the Sepolia cutover the configs still hold placeholders, so a
		// resolve failure in shadow mode is expected and must not wedge the
		// tick. Outside shadow mode it means a live keeper is misconfigured,
		// which has to fail loudly.
		if config.ShadowMode {
			logger.Warn("W1 queue-keeper config does not resolve yet", "error", err.Error())
			return &Result{
				Bound:      false,
				ShadowMode: true,
				Action:     queue.ActionNone.String(),
				Message:    "config unresolved: " + err.Error(),
			}, nil
		}
		return nil, err
	}

	tag := evmread.BlockFinalized
	if config.BlockTag == string(evmread.BlockLatest) {
		tag = evmread.BlockLatest
	}
	caller := evmread.New(runtime, deployment.Chain.Selector, tag)

	// CRE allows 15 contract reads per execution; the plan is budgeted up
	// front so a deep scan degrades to a shallow one instead of aborting the
	// tick (see reads.go).
	budget := evmread.NewBudget(reservedReads)

	pre, err := readPreamble(caller, deployment.Registry, deployment.Receiver, budget)
	if err != nil {
		return nil, err
	}
	rc := pre.receiver

	// The receiver's immutable is the authority; config is a mirror that can
	// drift across a redeploy (docs/envelope.md).
	if rc.ChainSelector != deployment.Chain.Selector {
		return nil, fmt.Errorf(
			"receiver %s reports CHAIN_SELECTOR %d but config resolves to %s (%d) — wrong receiver address or wrong chain",
			deployment.Receiver, rc.ChainSelector, deployment.Chain.Name, deployment.Chain.Selector)
	}

	// The observed block's clock, not the DON's wall clock: the receiver
	// rejects observedAt above block.timestamp outright, and batch ages are
	// only comparable against the clock the contract recorded them with.
	now := pre.blockTimestamp

	maxScan := config.MaxBatchScan
	if maxScan == 0 {
		maxScan = defaultMaxBatchScan
	}

	state, err := readQueueState(caller, pre, maxScan, budget)
	if err != nil {
		return nil, err
	}

	decision, err := queue.Decide(state)
	if err != nil {
		return nil, err
	}

	// Cross-check against the gas-bounded on-chain view. A failure here loses
	// the cross-check, not the decision — the keeper must keep working.
	divergenceClass := "unavailable"
	if onChain, statusErr := readUpkeepStatus(caller, deployment.Receiver, budget); statusErr != nil {
		logger.Warn("queueUpkeepStatus cross-check unavailable", "error", statusErr.Error())
	} else {
		d := queue.Classify(decision, onChain, state)
		divergenceClass = string(d.Class)
		attrs := append(d.LogAttrs(), "scanTruncated", state.ScanTruncated())
		if d.Unexplained() {
			logger.Error("W1 divergence from on-chain view is unexplained", attrs...)
		} else {
			logger.Info("W1 cross-check", attrs...)
		}
	}

	result := &Result{
		Bound:        true,
		ShadowMode:   deployment.ShadowMode,
		Action:       decision.Action.String(),
		BatchID:      decision.BatchID,
		EndIndex:     decision.EndIndex,
		Divergence:   divergenceClass,
		ScanTruncate: state.ScanTruncated(),
		Message:      decision.Reason,
	}

	// The address book is logged whole: when a divergence has to be triaged,
	// "which deployment was this tick actually talking to" is the first
	// question, and config only names the Registry.
	tickAttrs := append([]any{
		"chainName", deployment.Chain.Name,
		"queueExecutor", deployment.Receiver.Hex(),
	}, pre.protocol.LogAttrs()...)
	logger.Info("W1 queue-keeper tick", append(tickAttrs,
		"action", decision.Action.String(),
		"batchId", decision.BatchID,
		"endIndex", decision.EndIndex,
		"reason", decision.Reason,
		"paused", state.Paused,
		"currentBatchId", state.CurrentBatchID,
		"blockTimestamp", now,
		"cursor", state.NextBatchIDToProcess,
		"batchesScanned", len(state.Batches),
		"scanTruncated", state.ScanTruncated(),
		"readsRemaining", budget.Remaining(),
		"shadowMode", deployment.ShadowMode,
	)...)

	if decision.Action == queue.ActionNone {
		return result, nil
	}

	report, err := buildReport(decision, rc, deployment.Chain.Selector, now)
	if err != nil {
		return nil, err
	}

	// Refuse to emit a report the receiver would reject anyway. The guards are
	// the receiver's own, applied against its live state.
	if err := report.envelope.Validate(envelope.ReceiverState{
		ChainSelector: rc.ChainSelector,
		LastSequence:  rc.LastSequence,
		MaxReportAge:  rc.MaxReportAge,
	}, runtime.Now()); err != nil {
		return nil, fmt.Errorf("refusing to emit a report the receiver would reject: %w", err)
	}

	if deployment.ShadowMode {
		logger.Info("W1 shadow mode — report built but not written",
			"action", decision.Action.String(),
			"sequence", report.envelope.Sequence,
			"reportBytes", len(report.encoded),
		)
		result.Message = "shadow mode: " + decision.Reason
		return result, nil
	}

	txHash, err := writeReport(runtime, caller, deployment.Receiver, report.encoded)
	if err != nil {
		return nil, err
	}
	result.Wrote = true
	result.TxHash = txHash
	logger.Info("W1 wrote report", "action", decision.Action.String(), "txHash", txHash)

	return result, nil
}

// preparedReport pairs the encoded bytes with the envelope they came from, so
// the validation step does not have to decode what it just built.
type preparedReport struct {
	envelope envelope.Envelope
	encoded  []byte
}

func buildReport(d queue.Decision, rc receiverConfig, chainSelector, now uint64) (preparedReport, error) {
	// Sequence comes from the receiver's lastSequence read this tick, never
	// from a local counter (docs/envelope.md).
	sequence, err := envelope.NextSequence(rc.LastSequence)
	if err != nil {
		return preparedReport{}, err
	}

	r := queue.Report{ChainSelector: chainSelector, Sequence: sequence, ObservedAt: now}

	var encoded []byte
	switch d.Action {
	case queue.ActionPriceBatch:
		encoded, err = r.PriceBatch(d.BatchID)
	case queue.ActionProcessRequests:
		encoded, err = r.ProcessRequests(d.BatchID, d.EndIndex)
	case queue.ActionAdvanceCursor:
		encoded, err = r.AdvanceCursor(d.BatchID)
	default:
		return preparedReport{}, fmt.Errorf("%w: %s", queue.ErrUnknownAction, d.Action)
	}
	if err != nil {
		return preparedReport{}, err
	}

	decoded, err := envelope.Decode(encoded)
	if err != nil {
		return preparedReport{}, err
	}
	return preparedReport{envelope: decoded, encoded: encoded}, nil
}

func InitWorkflow(config *Config, _ *slog.Logger, _ cre.SecretsProvider) (cre.Workflow[*Config], error) {
	return cre.Workflow[*Config]{
		cre.Handler(
			cron.Trigger(&cron.Config{Schedule: config.Schedule}),
			onCronTrigger,
		),
	}, nil
}

func main() {
	wasm.NewRunner(cre.ParseJSON[Config]).Run(InitWorkflow)
}
