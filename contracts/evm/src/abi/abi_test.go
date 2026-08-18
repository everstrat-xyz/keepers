package abi_test

import (
	"strings"
	"testing"

	everabi "github.com/everstrat-xyz/keepers/contracts/evm/src/abi"
)

func TestAllABIsParse(t *testing.T) {
	for _, name := range everabi.All {
		t.Run(string(name), func(t *testing.T) {
			parsed, err := everabi.Get(name)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", name, err)
			}
			if len(parsed.Methods) == 0 {
				t.Errorf("%s parsed with no methods — wrong file vendored?", name)
			}
		})
	}
}

func TestGetUnknownABI(t *testing.T) {
	if _, err := everabi.Get("INotVendored"); err == nil {
		t.Error("Get() succeeded for an unvendored ABI, want error")
	}
}

// TestReceiverSurface pins the parts of the receiver ABIs the keepers actually
// call. A contracts-side rename would otherwise surface as a runtime pack
// failure inside a deployed workflow rather than a failing test here.
func TestReceiverSurface(t *testing.T) {
	tests := []struct {
		abi     everabi.Name
		methods []string
	}{
		{
			abi: everabi.ICREReceiverBase,
			methods: []string{
				"onReport",
				"lastSequence", "CHAIN_SELECTOR", "MAX_REPORT_AGE", "FORWARDER",
				"expectedWorkflowId", "expectedAuthor", "expectedWorkflowName",
			},
		},
		{
			abi: everabi.ICREQueueExecutor,
			methods: []string{
				"queueUpkeepStatus", "nextLiveBatchIdToProcess", "nextBatchIdToProcess",
				"affordableRequests", "minBatchAge", "maxUsersPerUpkeep",
			},
		},
		{
			abi: everabi.ICREStrategyExecutor,
			methods: []string{
				"strategyUpkeepStatus", "pendingRedemptionNeedsETH",
				"controllerReserveETH", "minDepositETH", "minWithdrawETH",
				"minHarvestETH", "syncInterval", "lastSyncAt",
				"exitLiquidityTargetETH", "minExitLiquidityTopUpETH",
			},
		},
		{
			abi:     everabi.IRegistry,
			methods: []string{"getContractByKey", "hasRole"},
		},
		{
			abi: everabi.IExitQueue,
			methods: []string{
				"currentBatchId", "batchInfo", "unprocessedUsers",
				"unprocessedUsersCount", "requestInfo", "MAX_BATCH_PROCESSING_TIME",
			},
		},
		{
			abi: everabi.IStrategyManager,
			methods: []string{
				// depositWeight gates _depositCapacityAvailable since contracts
				// PR #43 (R4-M-04); W2 reads it per strategy.
				"strategies", "depositWeight", "isStrategyInDepositCooldown",
				"pendingPerformanceFeeInETH", "performanceFeeBps",
			},
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.abi), func(t *testing.T) {
			for _, m := range tt.methods {
				if _, err := everabi.Method(tt.abi, m); err != nil {
					t.Errorf("%v", err)
				}
			}
		})
	}
}

// TestUpkeepStatusReturnShapes pins the cross-check views' return tuples. W1/W2
// decode these on every tick, and a silently-changed arity would misread the
// recommended action.
func TestUpkeepStatusReturnShapes(t *testing.T) {
	queueStatus, err := everabi.Method(everabi.ICREQueueExecutor, "queueUpkeepStatus")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(queueStatus.Outputs); got != 3 {
		t.Errorf("queueUpkeepStatus returns %d values, want 3 (action, batchId, count)", got)
	}
	if got := queueStatus.Outputs[0].Type.String(); got != "uint8" {
		t.Errorf("queueUpkeepStatus action type = %s, want uint8 (the QueueAction enum)", got)
	}

	strategyStatus, err := everabi.Method(everabi.ICREStrategyExecutor, "strategyUpkeepStatus")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strategyStatus.Outputs); got != 2 {
		t.Errorf("strategyUpkeepStatus returns %d values, want 2 (action, amount)", got)
	}
	if got := strategyStatus.Outputs[0].Type.String(); got != "uint8" {
		t.Errorf("strategyUpkeepStatus action type = %s, want uint8 (the StrategyAction enum)", got)
	}
}

// TestOnReportSignature is the one signature that must never drift: it is how
// the KeystoneForwarder reaches the receiver.
func TestOnReportSignature(t *testing.T) {
	m, err := everabi.Method(everabi.ICREReceiverBase, "onReport")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.Sig, "onReport(bytes,bytes)"; got != want {
		t.Errorf("onReport signature = %s, want %s", got, want)
	}
}

// TestControllerActualReturnShapes pins the R4-L-05 ABI change: Controller
// deposit/withdraw/harvest now return the StrategyManager actual, which
// CREStrategyExecutor emits on StrategyUpkeepPerformed. The keepers never call
// these (the receiver does), but a return-shape drift here means the vendored
// ABI is stale relative to the deployed executor.
//
// Overloads are keyed by name in the parsed ABI (go-ethereum renames the second
// occurrence), so this walks every entry and asserts none of the keeper-facing
// set is void.
func TestControllerActualReturnShapes(t *testing.T) {
	wantReturns := map[string]string{
		"depositToStrategies":                 "uint256",
		"depositToStrategy":                   "uint256",
		"withdrawFromStrategies":              "uint256",
		"withdrawFromStrategy":                "uint256",
		"harvestPerformanceFeeFromStrategy":   "uint256,uint256",
		"harvestPerformanceFeeFromStrategies": "uint256,uint256",
	}

	parsed, err := everabi.Get(everabi.IController)
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range parsed.Methods {
		base := strings.TrimRight(name, "0123456789")
		want, keeperFacing := wantReturns[base]
		if !keeperFacing {
			continue
		}
		var parts []string
		for _, o := range m.Outputs {
			parts = append(parts, o.Type.String())
		}
		if got := strings.Join(parts, ","); got != want {
			t.Errorf("%s returns (%s), want (%s) — a void return means the vendored ABI predates contracts PR #43", name, got, want)
		}
		if _, ok := wantReturns[name]; !ok && base != name {
			t.Logf("overload %s checked under %s", name, base)
		}
	}
}

func TestJSONReturnsRawBytes(t *testing.T) {
	raw, err := everabi.JSON(everabi.ICREReceiverBase)
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	if len(raw) == 0 {
		t.Error("JSON() returned no bytes")
	}
}
