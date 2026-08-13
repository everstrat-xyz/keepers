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
	"github.com/everstrat-xyz/keepers/pkg/strategy"
)

// Config is loaded from config.<target>.json (see workflow.yaml).
type Config struct {
	chains.Config
	Schedule                string `json:"schedule"`
	StrategyExecutorAddress string `json:"strategyExecutorAddress"`

	// BlockTag selects which block reads observe. Defaults to "finalized",
	// which is the only setting safe for DON consensus; "latest" exists for
	// local simulation against a single node.
	BlockTag string `json:"blockTag"`
}

// Result is the tick's structured outcome; shadow-mode monitoring reads it.
type Result struct {
	Bound         bool   `json:"bound"`
	ShadowMode    bool   `json:"shadowMode"`
	Action        string `json:"action"`
	Amount        string `json:"amount"`
	Divergence    string `json:"divergence"`
	Wrote         bool   `json:"wrote"`
	TxHash        string `json:"txHash,omitempty"`
	ScanTruncated bool   `json:"scanTruncated"`
	Message       string `json:"message"`
}

func onCronTrigger(config *Config, runtime cre.Runtime, _ *cron.Payload) (*Result, error) {
	logger := runtime.Logger()

	deployment, err := chains.Resolve(config.Config, config.StrategyExecutorAddress)
	if err != nil {
		// Same rule as W1: placeholders are expected in shadow mode, but a
		// live keeper that cannot resolve its config must fail loudly.
		if config.ShadowMode {
			logger.Warn("W2 strategy-keeper config does not resolve yet", "error", err.Error())
			return &Result{
				Bound:      false,
				ShadowMode: true,
				Action:     strategy.ActionNone.String(),
				Amount:     "0",
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
	budget := evmread.NewBudget(reservedReads)

	pre, err := readPreamble(caller, deployment.Registry, deployment.Receiver, budget)
	if err != nil {
		return nil, err
	}
	rc := pre.receiver

	if rc.ChainSelector != deployment.Chain.Selector {
		return nil, fmt.Errorf(
			"receiver %s reports CHAIN_SELECTOR %d but config resolves to %s (%d) — wrong receiver address or wrong chain",
			deployment.Receiver, rc.ChainSelector, deployment.Chain.Name, deployment.Chain.Selector)
	}

	state, err := readStrategyState(caller, pre, budget)
	if err != nil {
		return nil, err
	}

	decision, err := strategy.Decide(state)
	if err != nil {
		return nil, err
	}

	divergenceClass := "unavailable"
	if onChain, statusErr := readUpkeepStatus(caller, deployment.Receiver, budget); statusErr != nil {
		logger.Warn("strategyUpkeepStatus cross-check unavailable", "error", statusErr.Error())
	} else {
		d := strategy.Classify(decision, onChain, state)
		divergenceClass = string(d.Class)
		if d.Unexplained() {
			logger.Error("W2 divergence from on-chain view is unexplained", d.LogAttrs()...)
		} else {
			logger.Info("W2 cross-check", d.LogAttrs()...)
		}
	}

	result := &Result{
		Bound:         true,
		ShadowMode:    deployment.ShadowMode,
		Action:        decision.Action.String(),
		Amount:        decision.Amount.String(),
		Divergence:    divergenceClass,
		ScanTruncated: state.ScanTruncated,
		Message:       decision.Reason,
	}

	logger.Info("W2 strategy-keeper tick",
		"chainName", deployment.Chain.Name,
		"strategyExecutor", deployment.Receiver.Hex(),
		"controller", pre.addrs.Controller.Hex(),
		"strategyManager", pre.addrs.StrategyManager.Hex(),
		"action", decision.Action.String(),
		// Diagnostic only — the report carries no amount.
		"amount", decision.Amount.String(),
		"reason", decision.Reason,
		"paused", state.Paused,
		"strategies", len(state.Strategies),
		"pendingNeedsETH", state.NeedsETH.String(),
		"controllerBalance", state.ControllerBalance.String(),
		"blockTimestamp", state.Now,
		"scanTruncated", state.ScanTruncated,
		"readsRemaining", budget.Remaining(),
		"shadowMode", deployment.ShadowMode,
	)

	if decision.Action == strategy.ActionNone {
		return result, nil
	}

	report, err := buildReport(decision, rc, deployment.Chain.Selector, state.Now)
	if err != nil {
		return nil, err
	}

	if err := report.envelope.Validate(envelope.ReceiverState{
		ChainSelector: rc.ChainSelector,
		LastSequence:  rc.LastSequence,
		MaxReportAge:  rc.MaxReportAge,
	}, runtime.Now()); err != nil {
		return nil, fmt.Errorf("refusing to emit a report the receiver would reject: %w", err)
	}

	if deployment.ShadowMode {
		logger.Info("W2 shadow mode — report built but not written",
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
	logger.Info("W2 wrote report", "action", decision.Action.String(), "txHash", txHash)

	return result, nil
}

// preparedReport pairs the encoded bytes with the envelope they came from.
type preparedReport struct {
	envelope envelope.Envelope
	encoded  []byte
}

func buildReport(d strategy.Decision, rc receiverConfig, chainSelector, now uint64) (preparedReport, error) {
	sequence, err := envelope.NextSequence(rc.LastSequence)
	if err != nil {
		return preparedReport{}, err
	}

	// Build takes an action and nothing else — the receiver recomputes every
	// amount, so there is no params argument to get wrong.
	encoded, err := strategy.Report{
		ChainSelector: chainSelector,
		Sequence:      sequence,
		ObservedAt:    now,
	}.Build(d.Action)
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
