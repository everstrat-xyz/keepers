//go:build wasip1

package main

import (
	"log/slog"

	"github.com/smartcontractkit/cre-sdk-go/capabilities/scheduler/cron"
	"github.com/smartcontractkit/cre-sdk-go/cre"
	"github.com/smartcontractkit/cre-sdk-go/cre/wasm"

	"github.com/everstrat-xyz/keepers/pkg/chains"
	"github.com/everstrat-xyz/keepers/pkg/strategy"
)

// Config is loaded from config.<target>.json (see workflow.yaml).
//
// The chain-related fields are shared with W1 and validated by pkg/chains;
// strategyExecutorAddress is W2's own receiver. Addresses are public config,
// never secrets — see secrets.yaml.
type Config struct {
	chains.Config
	Schedule                string `json:"schedule"`
	StrategyExecutorAddress string `json:"strategyExecutorAddress"`
}

type Result struct {
	// Bound reports whether the config resolves to a real deployment. It stays
	// false while the scaffold configs carry zero-address placeholders.
	Bound      bool   `json:"bound"`
	ShadowMode bool   `json:"shadowMode"`
	Message    string `json:"message"`
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
				Message:    "strategy-keeper scaffold — config unresolved: " + err.Error(),
			}, nil
		}
		return nil, err
	}

	priority := make([]string, len(strategy.Priority))
	for i, a := range strategy.Priority {
		priority[i] = a.String()
	}

	logger.Info("W2 strategy-keeper scaffold tick",
		"chainName", deployment.Chain.Name,
		"chainSelector", deployment.Chain.Selector,
		"forwarder", deployment.Chain.Forwarder.Hex(),
		"registry", deployment.Registry.Hex(),
		"strategyExecutor", deployment.Receiver.Hex(),
		"shadowMode", deployment.ShadowMode,
		"maxReportAgeSeconds", deployment.MaxReportAgeSeconds,
		"actionPriority", priority,
	)

	// Report construction lands with the W2 business logic (issue #4): it needs
	// on-chain reads for lastSequence and the upkeep decision, which this
	// scaffold does not perform. pkg/strategy already encodes the report bytes.
	return &Result{
		Bound:      true,
		ShadowMode: deployment.ShadowMode,
		Message:    "strategy-keeper scaffold — business logic not implemented",
	}, nil
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
