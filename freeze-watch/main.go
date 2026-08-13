//go:build wasip1

// Command freeze-watch is W4: observability for freeze precursors and keeper
// health.
//
// # It cannot write on-chain
//
// W4 reads and notifies. There is no `writeReport` path here — this package
// imports neither pkg/crewrite nor pkg/envelope, so actuation would require
// adding an import that shows up in review. NAV-guardian pause actuation is a
// separate epic gated on DAO sign-off (TECH_SPEC Phase 3).
package main

import (
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/smartcontractkit/cre-sdk-go/capabilities/networking/http"
	"github.com/smartcontractkit/cre-sdk-go/capabilities/scheduler/cron"
	"github.com/smartcontractkit/cre-sdk-go/cre"
	"github.com/smartcontractkit/cre-sdk-go/cre/wasm"

	"github.com/everstrat-xyz/keepers/pkg/chains"
	"github.com/everstrat-xyz/keepers/pkg/evmread"
	"github.com/everstrat-xyz/keepers/pkg/freezewatch"
)

// alertWebhookSecret is the secret name declared in secrets.yaml. The URL is a
// secret because it is a capability: anyone holding it can post to the ops
// channel.
const alertWebhookSecret = "ALERT_WEBHOOK_URL"

// Config is loaded from config.<target>.json (see workflow.yaml).
type Config struct {
	chains.Config
	Schedule string `json:"schedule"`

	// Receiver addresses, watched for liveness. Optional: a zero or absent
	// address is skipped rather than failing the tick, since W4 should keep
	// working through a partial deployment.
	QueueExecutorAddress    string `json:"queueExecutorAddress"`
	StrategyExecutorAddress string `json:"strategyExecutorAddress"`

	// Thresholds override freezewatch.DefaultThresholds when non-zero.
	OracleStaleAfterSeconds      uint64 `json:"oracleStaleAfterSeconds"`
	EscapeHatchWarnWithinSeconds uint64 `json:"escapeHatchWarnWithinSeconds"`
	KeeperStalledAfterSeconds    uint64 `json:"keeperStalledAfterSeconds"`
	BacklogWarnBatches           uint64 `json:"backlogWarnBatches"`

	// DryRun evaluates and logs alerts without posting the webhook.
	DryRun bool `json:"dryRun"`

	BlockTag string `json:"blockTag"`
}

func (c *Config) thresholds() freezewatch.Thresholds {
	t := freezewatch.DefaultThresholds()
	if c.OracleStaleAfterSeconds > 0 {
		t.OracleStaleAfter = c.OracleStaleAfterSeconds
	}
	if c.EscapeHatchWarnWithinSeconds > 0 {
		t.EscapeHatchWarnWithin = c.EscapeHatchWarnWithinSeconds
	}
	if c.KeeperStalledAfterSeconds > 0 {
		t.KeeperStalledAfter = c.KeeperStalledAfterSeconds
	}
	if c.BacklogWarnBatches > 0 {
		t.BacklogWarnBatches = c.BacklogWarnBatches
	}
	return t
}

// Result is the tick's structured outcome.
type Result struct {
	Bound    bool     `json:"bound"`
	Critical int      `json:"critical"`
	Warning  int      `json:"warning"`
	Notified bool     `json:"notified"`
	DryRun   bool     `json:"dryRun"`
	Alerts   []string `json:"alerts"`
	Message  string   `json:"message"`
}

func onCronTrigger(config *Config, runtime cre.Runtime, _ *cron.Payload) (*Result, error) {
	logger := runtime.Logger()

	chain, err := chains.ByName(config.ChainName)
	if err != nil {
		return nil, err
	}
	registryAddr, err := chains.ParseAddress("registryAddress", config.RegistryAddress)
	if err != nil {
		// Before deployment the config holds placeholders. W4 is
		// observability, so a missing target is a quiet no-op rather than a
		// failing tick that would itself page someone.
		logger.Warn("W4 freeze-watch has no registry to watch yet", "error", err.Error())
		return &Result{Bound: false, Message: "config unresolved: " + err.Error()}, nil
	}

	tag := evmread.BlockFinalized
	if config.BlockTag == string(evmread.BlockLatest) {
		tag = evmread.BlockLatest
	}
	caller := evmread.New(runtime, chain.Selector, tag)
	budget := evmread.NewBudget(0)

	obs, err := readObservation(caller, config, registryAddr, budget)
	if err != nil {
		return nil, err
	}

	alerts := freezewatch.Evaluate(obs, config.thresholds())
	summary := freezewatch.Summarize(alerts)

	rendered := make([]string, len(alerts))
	for i, a := range alerts {
		rendered[i] = a.String()
	}

	logger.Info("W4 freeze-watch tick",
		"chainName", chain.Name,
		"registry", registryAddr.Hex(),
		"critical", summary.Critical,
		"warning", summary.Warning,
		"blockTimestamp", obs.Now,
		"batchesWatched", len(obs.Batches),
		"feedsWatched", len(obs.Feeds),
		"strategiesWatched", len(obs.Strategies),
		"readsRemaining", budget.Remaining(),
	)
	for _, a := range alerts {
		if a.Severity == freezewatch.SeverityCritical {
			logger.Error("W4 alert", "kind", string(a.Kind), "subject", a.Subject, "message", a.Message)
		} else {
			logger.Warn("W4 alert", "kind", string(a.Kind), "subject", a.Subject, "message", a.Message)
		}
	}

	result := &Result{
		Bound:    true,
		Critical: summary.Critical,
		Warning:  summary.Warning,
		DryRun:   config.DryRun,
		Alerts:   rendered,
		Message:  summary.Subject(),
	}

	if !freezewatch.ShouldNotify(alerts) {
		return result, nil
	}
	if config.DryRun {
		logger.Info("W4 dry run — alerts evaluated but no webhook posted", "count", len(alerts))
		return result, nil
	}

	if err := notify(config, runtime, alerts, chain.Name, obs.Now); err != nil {
		// A failed notification must not fail the tick: the alerts are already
		// in the logs, and a workflow that errors here would look identical to
		// one that found nothing.
		logger.Error("W4 could not post alerts", "error", err.Error(), "count", len(alerts))
		result.Message = summary.Subject() + " (webhook failed: " + err.Error() + ")"
		return result, nil
	}

	result.Notified = true
	return result, nil
}

// notify posts the alert digest to the configured webhook.
//
// The HTTP capability runs in node mode, so the call is made per node and the
// result agreed by consensus — a single node cannot fabricate a delivery.
func notify(config *Config, runtime cre.Runtime, alerts []freezewatch.Alert, chain string, at uint64) error {
	secret, err := runtime.GetSecret(&cre.SecretRequest{Id: alertWebhookSecret}).Await()
	if err != nil {
		return fmt.Errorf("reading %s: %w", alertWebhookSecret, err)
	}
	if secret.Value == "" {
		return fmt.Errorf("%s is empty", alertWebhookSecret)
	}

	body, err := freezewatch.MarshalPayload(freezewatch.BuildPayload(alerts, chain, at))
	if err != nil {
		return err
	}

	client := &http.Client{}
	status, err := http.SendRequest(
		body,
		runtime,
		client,
		func(payload []byte, _ *slog.Logger, sender *http.SendRequester) (uint32, error) {
			resp, err := sender.SendRequest(&http.Request{
				Url:     secret.Value,
				Method:  "POST",
				Body:    payload,
				Headers: map[string]string{"Content-Type": "application/json"},
				Timeout: durationpb.New(10 * time.Second),
			}).Await()
			if err != nil {
				return 0, err
			}
			return resp.StatusCode, nil
		},
		cre.ConsensusIdenticalAggregation[uint32](),
	).Await()
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", status)
	}
	return nil
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
