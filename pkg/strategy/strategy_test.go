package strategy_test

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/everstrat-xyz/keepers/pkg/envelope"
	"github.com/everstrat-xyz/keepers/pkg/strategy"
)

const sepolia = uint64(16015286601757825753)

// TestActionOrdinalsMatchSolidity pins the enum against
// ICREStrategyExecutor.StrategyAction, including the fact that
// ProvideExitLiquidity was appended last rather than inserted in priority
// order.
func TestActionOrdinalsMatchSolidity(t *testing.T) {
	for _, tt := range []struct {
		action strategy.Action
		want   uint8
		name   string
	}{
		{strategy.ActionNone, 0, "None"},
		{strategy.ActionRebalance, 1, "Rebalance"},
		{strategy.ActionWithdrawShortfall, 2, "WithdrawShortfall"},
		{strategy.ActionDepositExcess, 3, "DepositExcess"},
		{strategy.ActionHarvestPerformanceFees, 4, "HarvestPerformanceFees"},
		{strategy.ActionSync, 5, "Sync"},
		{strategy.ActionProvideExitLiquidity, 6, "ProvideExitLiquidity"},
	} {
		if uint8(tt.action) != tt.want {
			t.Errorf("%s = %d, want %d", tt.name, uint8(tt.action), tt.want)
		}
		if tt.action.String() != tt.name {
			t.Errorf("String() = %q, want %q", tt.action.String(), tt.name)
		}
	}

	if strategy.ActionNone.Valid() {
		t.Error("ActionNone.Valid() = true; None is not a deliverable action")
	}
	if strategy.Action(7).Valid() {
		t.Error("Action(7).Valid() = true; there is no seventh StrategyAction")
	}
}

// TestPriorityMatchesUpkeepStatus mirrors the branch order in
// CREStrategyExecutor.strategyUpkeepStatus. If a workflow proposes actions in a
// different order than the contract's view recommends them, shadow-mode
// divergence monitoring reports false positives.
func TestPriorityMatchesUpkeepStatus(t *testing.T) {
	want := []strategy.Action{
		strategy.ActionRebalance,
		strategy.ActionWithdrawShortfall,
		strategy.ActionProvideExitLiquidity,
		strategy.ActionDepositExcess,
		strategy.ActionHarvestPerformanceFees,
		strategy.ActionSync,
	}
	if len(strategy.Priority) != len(want) {
		t.Fatalf("Priority has %d entries, want %d", len(strategy.Priority), len(want))
	}
	for i := range want {
		if strategy.Priority[i] != want[i] {
			t.Errorf("Priority[%d] = %s, want %s", i, strategy.Priority[i], want[i])
		}
	}

	// Every actionable enum value must appear exactly once.
	seen := map[strategy.Action]int{}
	for _, a := range strategy.Priority {
		seen[a]++
	}
	for a := strategy.ActionRebalance; a <= strategy.ActionProvideExitLiquidity; a++ {
		if seen[a] != 1 {
			t.Errorf("%s appears %d times in Priority, want 1", a, seen[a])
		}
	}
}

// TestBuildCarriesNoParams is the hard-constraint test for W2: the contract
// recomputes every amount, so the report must be action-only. There is no API
// to attach params, and the built bytes must confirm it.
func TestBuildCarriesNoParams(t *testing.T) {
	r := strategy.Report{ChainSelector: sepolia, Sequence: 3, ObservedAt: 1700000000}

	for _, action := range strategy.Priority {
		t.Run(action.String(), func(t *testing.T) {
			report, err := r.Build(action)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			e, err := envelope.Decode(report)
			if err != nil {
				t.Fatalf("envelope.Decode() error = %v", err)
			}
			if strategy.Action(e.Action) != action {
				t.Errorf("action = %s, want %s", strategy.Action(e.Action), action)
			}
			if len(e.Params) != 0 {
				t.Errorf("params = 0x%x (%d bytes), want empty", e.Params, len(e.Params))
			}
			if err := strategy.ValidateParams(e.Params); err != nil {
				t.Errorf("ValidateParams() error = %v, want nil", err)
			}
		})
	}
}

func TestValidateParamsRejectsAnyPayload(t *testing.T) {
	// A one-ETH word — the exact mistake the constraint forbids.
	oneETH, err := hex.DecodeString("0000000000000000000000000000000000000000000000000de0b6b3a7640000")
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name   string
		params []byte
	}{
		{"one ETH word", oneETH},
		{"single byte", []byte{0x00}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := strategy.ValidateParams(tt.params); !errors.Is(err, strategy.ErrParamsNotEmpty) {
				t.Errorf("ValidateParams() error = %v, want %v", err, strategy.ErrParamsNotEmpty)
			}
		})
	}

	if err := strategy.ValidateParams(nil); err != nil {
		t.Errorf("ValidateParams(nil) error = %v, want nil", err)
	}
	if err := strategy.ValidateParams([]byte{}); err != nil {
		t.Errorf("ValidateParams(empty) error = %v, want nil", err)
	}
}

func TestBuildRejectsNonActionableActions(t *testing.T) {
	r := strategy.Report{ChainSelector: sepolia, Sequence: 1, ObservedAt: 1700000000}

	for _, a := range []strategy.Action{strategy.ActionNone, strategy.Action(7), strategy.Action(255)} {
		if _, err := r.Build(a); !errors.Is(err, strategy.ErrUnknownAction) {
			t.Errorf("Build(%s) error = %v, want %v", a, err, strategy.ErrUnknownAction)
		}
	}
}

// TestBuildMatchesEnvelopeFixture pins a full W2 report against the Solidity
// golden bytes in pkg/envelope/testdata (fixture
// "strategy_provide_exit_liquidity").
func TestBuildMatchesEnvelopeFixture(t *testing.T) {
	const want = "0x0000000000000000000000000000000000000000000000000000000000000020" +
		"000000000000000000000000000000000000000000000000de41ba4fc9d91ad9" +
		"0000000000000000000000000000000000000000000000000000000000000007" +
		"000000000000000000000000000000000000000000000000000000006553f1b4" +
		"0000000000000000000000000000000000000000000000000000000000000006" +
		"00000000000000000000000000000000000000000000000000000000000000a0" +
		"0000000000000000000000000000000000000000000000000000000000000000"

	r := strategy.Report{ChainSelector: sepolia, Sequence: 7, ObservedAt: 1700000180}
	got, err := r.Build(strategy.ActionProvideExitLiquidity)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if "0x"+hex.EncodeToString(got) != want {
		t.Errorf("report = 0x%x\nwant    %s", got, want)
	}
}

func TestBuildRejectsBadHeaders(t *testing.T) {
	r := strategy.Report{ChainSelector: sepolia, Sequence: 0, ObservedAt: 1700000000}
	if _, err := r.Build(strategy.ActionSync); !errors.Is(err, envelope.ErrZeroSequence) {
		t.Errorf("error = %v, want %v", err, envelope.ErrZeroSequence)
	}
}
