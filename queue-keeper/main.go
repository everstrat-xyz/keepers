//go:build wasip1

package main

import (
	"log/slog"

	"github.com/smartcontractkit/cre-sdk-go/capabilities/scheduler/cron"
	"github.com/smartcontractkit/cre-sdk-go/cre"
	"github.com/smartcontractkit/cre-sdk-go/cre/wasm"

	"github.com/everstrat-xyz/keepers/pkg/chains"
	"github.com/everstrat-xyz/keepers/pkg/queue"
)

// Config is loaded from config.<target>.json (see workflow.yaml).
//
// The chain-related fields are shared with W2 and validated by pkg/chains;
// queueExecutorAddress is W1's own receiver. Addresses are public config, never
// secrets — see secrets.yaml.
type Config struct {
	chains.Config
	Schedule             string `json:"schedule"`
	QueueExecutorAddress string `json:"queueExecutorAddress"`
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
				Message:    "queue-keeper scaffold — config unresolved: " + err.Error(),
			}, nil
		}
		return nil, err
	}

	logger.Info("W1 queue-keeper scaffold tick",
		"chainName", deployment.Chain.Name,
		"chainSelector", deployment.Chain.Selector,
		"forwarder", deployment.Chain.Forwarder.Hex(),
		"registry", deployment.Registry.Hex(),
		"queueExecutor", deployment.Receiver.Hex(),
		"shadowMode", deployment.ShadowMode,
		"maxReportAgeSeconds", deployment.MaxReportAgeSeconds,
		"actions", []string{
			queue.ActionPriceBatch.String(),
			queue.ActionProcessRequests.String(),
			queue.ActionAdvanceCursor.String(),
		},
	)

	// Report construction lands with the W1 business logic (issue #3): it needs
	// on-chain reads for lastSequence and the affordable range, which this
	// scaffold does not perform. pkg/queue already encodes the report bytes.
	return &Result{
		Bound:      true,
		ShadowMode: deployment.ShadowMode,
		Message:    "queue-keeper scaffold — business logic not implemented",
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
