//go:build wasip1

package main

import (
	"log/slog"

	"github.com/smartcontractkit/cre-sdk-go/capabilities/scheduler/cron"
	"github.com/smartcontractkit/cre-sdk-go/cre"
	"github.com/smartcontractkit/cre-sdk-go/cre/wasm"
)

// Config is loaded from config.<target>.json (see workflow.yaml).
// Shared Envelope/ABI helpers land in a later issue; keep addresses out of secrets.
type Config struct {
	Schedule               string `json:"schedule"`
	ChainName              string `json:"chainName"`
	ChainSelector          string `json:"chainSelector"`
	RegistryAddress        string `json:"registryAddress"`
	QueueExecutorAddress   string `json:"queueExecutorAddress"`
	ShadowMode             bool   `json:"shadowMode"`
	MaxReportAgeSeconds    uint64 `json:"maxReportAgeSeconds"`
}

type Result struct {
	ShadowMode bool   `json:"shadowMode"`
	Message    string `json:"message"`
}

func onCronTrigger(config *Config, runtime cre.Runtime, _ *cron.Payload) (*Result, error) {
	logger := runtime.Logger()
	logger.Info("W1 queue-keeper scaffold tick",
		"chainName", config.ChainName,
		"chainSelector", config.ChainSelector,
		"registry", config.RegistryAddress,
		"queueExecutor", config.QueueExecutorAddress,
		"shadowMode", config.ShadowMode,
		"maxReportAgeSeconds", config.MaxReportAgeSeconds,
	)
	return &Result{
		ShadowMode: config.ShadowMode,
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
