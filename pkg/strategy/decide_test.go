package strategy_test

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/everstrat-xyz/keepers/pkg/strategy"
)

const oneETH = 1_000_000_000_000_000_000

func eth(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(oneETH)) }

func addr(n byte) common.Address {
	var a common.Address
	a[19] = n
	return a
}

// healthyStrategy has capacity both ways and no accrued fee.
func healthyStrategy(n byte) strategy.Strategy {
	return strategy.Strategy{
		Address:                  addr(n),
		Paused:                   false,
		Healthy:                  true,
		MaxDeposit:               eth(100),
		MaxWithdrawal:            eth(100),
		InDepositCooldown:        false,
		PendingPerformanceFeeETH: new(big.Int),
	}
}

// idleState is a healthy protocol with nothing to do: balance exactly at the
// reserve, float at target, no fees, sync not yet due.
func idleState() strategy.State {
	return strategy.State{
		Now:                      1_700_000_000,
		Paused:                   false,
		ControllerBalance:        eth(1),
		NeedsETH:                 new(big.Int),
		AMMFreeBalance:           eth(5),
		Strategies:               []strategy.Strategy{healthyStrategy(1)},
		PerformanceFeeBps:        big.NewInt(1000),
		ControllerReserveETH:     eth(1),
		MinDepositETH:            eth(1),
		MinWithdrawETH:           new(big.Int).Div(eth(1), big.NewInt(100)),
		MinHarvestETH:            new(big.Int).Div(eth(1), big.NewInt(100)),
		ExitLiquidityTargetETH:   eth(5),
		MinExitLiquidityTopUpETH: new(big.Int).Div(eth(1), big.NewInt(100)),
		SyncInterval:             86400,
		LastSyncAt:               1_700_000_000,
	}
}

func TestDecideIdle(t *testing.T) {
	d, err := strategy.Decide(idleState())
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != strategy.ActionNone {
		t.Errorf("Action = %s (%s), want None", d.Action, d.Reason)
	}
}

func TestDecidePaused(t *testing.T) {
	s := idleState()
	s.Paused = true
	// Give it work it would otherwise take.
	s.Strategies[0].Healthy = false

	d, err := strategy.Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != strategy.ActionNone {
		t.Errorf("Action = %s, want None while paused", d.Action)
	}
}

// TestDecidePriorityOrder walks the ladder: each case sets up the conditions
// for its action *and* every lower-priority one, so a mis-ordered branch shows
// up as the wrong action rather than passing by luck.
func TestDecidePriorityOrder(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*strategy.State)
		want strategy.Action
	}{
		{
			name: "unhealthy strategy wins over everything",
			mut: func(s *strategy.State) {
				s.Strategies[0].Healthy = false
				s.ControllerBalance = eth(100)  // would also deposit
				s.NeedsETH = eth(500)           // would also withdraw
				s.LastSyncAt = s.Now - 200000   // would also sync
				s.AMMFreeBalance = new(big.Int) // would also top up
			},
			want: strategy.ActionRebalance,
		},
		{
			name: "shortfall beats exit liquidity and deposits",
			mut: func(s *strategy.State) {
				s.ControllerBalance = eth(1)
				s.NeedsETH = eth(50)
				s.AMMFreeBalance = new(big.Int)
			},
			want: strategy.ActionWithdrawShortfall,
		},
		{
			name: "exit liquidity beats deposits",
			mut: func(s *strategy.State) {
				s.ControllerBalance = eth(100)
				s.AMMFreeBalance = eth(1) // 4 ETH below the 5 ETH target
			},
			want: strategy.ActionProvideExitLiquidity,
		},
		{
			name: "deposit excess when the float is already at target",
			mut: func(s *strategy.State) {
				s.ControllerBalance = eth(100)
				s.AMMFreeBalance = eth(5)
			},
			want: strategy.ActionDepositExcess,
		},
		{
			name: "harvest when there is no excess to deposit",
			mut: func(s *strategy.State) {
				s.Strategies[0].PendingPerformanceFeeETH = eth(2)
			},
			want: strategy.ActionHarvestPerformanceFees,
		},
		{
			name: "sync when nothing else is due",
			mut: func(s *strategy.State) {
				s.LastSyncAt = s.Now - 90000
			},
			want: strategy.ActionSync,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := idleState()
			tt.mut(&s)
			d, err := strategy.Decide(s)
			if err != nil {
				t.Fatal(err)
			}
			if d.Action != tt.want {
				t.Errorf("Action = %s (%s), want %s", d.Action, d.Reason, tt.want)
			}
		})
	}
}

// TestDecideMatchesContractGuards covers the conditions that gate each branch,
// where an off-by-one produces a report the receiver rejects.
func TestDecideMatchesContractGuards(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*strategy.State)
		want strategy.Action
	}{
		{
			name: "a paused unhealthy strategy does not trigger rebalance",
			mut:  func(s *strategy.State) { s.Strategies[0].Healthy = false; s.Strategies[0].Paused = true },
			want: strategy.ActionNone,
		},
		{
			name: "shortfall below minWithdrawETH is skipped",
			mut: func(s *strategy.State) {
				s.ControllerBalance = eth(1)
				// 0.001 ETH short, below the 0.01 ETH floor.
				s.NeedsETH = new(big.Int).Add(eth(1), big.NewInt(oneETH/1000))
			},
			want: strategy.ActionNone,
		},
		{
			name: "shortfall with no withdrawable capacity is skipped",
			mut: func(s *strategy.State) {
				s.ControllerBalance = eth(1)
				s.NeedsETH = eth(50)
				s.Strategies[0].MaxWithdrawal = new(big.Int)
			},
			want: strategy.ActionNone,
		},
		{
			name: "zero exit-liquidity target disables the top-up branch",
			mut: func(s *strategy.State) {
				s.ExitLiquidityTargetETH = new(big.Int)
				s.AMMFreeBalance = new(big.Int)
				s.ControllerBalance = eth(100)
			},
			want: strategy.ActionDepositExcess,
		},
		{
			name: "deposit is skipped when every strategy is in cooldown",
			mut: func(s *strategy.State) {
				s.ControllerBalance = eth(100)
				s.Strategies[0].InDepositCooldown = true
			},
			want: strategy.ActionNone,
		},
		{
			name: "deposit is skipped when no strategy has capacity",
			mut: func(s *strategy.State) {
				s.ControllerBalance = eth(100)
				s.Strategies[0].MaxDeposit = new(big.Int)
			},
			want: strategy.ActionNone,
		},
		{
			name: "zero performanceFeeBps suppresses harvest",
			mut: func(s *strategy.State) {
				s.PerformanceFeeBps = new(big.Int)
				s.Strategies[0].PendingPerformanceFeeETH = eth(2)
			},
			want: strategy.ActionNone,
		},
		{
			name: "fees below minHarvestETH are skipped",
			mut: func(s *strategy.State) {
				s.Strategies[0].PendingPerformanceFeeETH = big.NewInt(oneETH / 1000)
			},
			want: strategy.ActionNone,
		},
		{
			name: "zero syncInterval disables sync",
			mut: func(s *strategy.State) {
				s.SyncInterval = 0
				s.LastSyncAt = s.Now - 999999
			},
			want: strategy.ActionNone,
		},
		{
			name: "sync is skipped with no registered strategies",
			mut: func(s *strategy.State) {
				s.Strategies = nil
				s.LastSyncAt = s.Now - 999999
			},
			want: strategy.ActionNone,
		},
		{
			name: "sync exactly at the interval fires",
			mut: func(s *strategy.State) {
				s.LastSyncAt = s.Now - s.SyncInterval
			},
			want: strategy.ActionSync,
		},
		{
			name: "sync one second before the interval does not",
			mut: func(s *strategy.State) {
				s.LastSyncAt = s.Now - s.SyncInterval + 1
			},
			want: strategy.ActionNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := idleState()
			tt.mut(&s)
			d, err := strategy.Decide(s)
			if err != nil {
				t.Fatal(err)
			}
			if d.Action != tt.want {
				t.Errorf("Action = %s (%s), want %s", d.Action, d.Reason, tt.want)
			}
		})
	}
}

// TestExitLiquidityTopUpIsCappedByIdleExcess reproduces _exitLiquidityTopUp's
// min(): the Controller cannot top the float up with ETH it does not have
// spare, and over-claiming would revert.
func TestExitLiquidityTopUpIsCappedByIdleExcess(t *testing.T) {
	s := idleState()
	s.AMMFreeBalance = new(big.Int) // 5 ETH short of target
	s.ControllerBalance = eth(3)    // reserve is 1 ETH, so 2 ETH spare
	s.NeedsETH = new(big.Int)

	d, err := strategy.Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != strategy.ActionProvideExitLiquidity {
		t.Fatalf("Action = %s, want ProvideExitLiquidity", d.Action)
	}
	if want := eth(2); d.Amount.Cmp(want) != 0 {
		t.Errorf("Amount = %s, want %s (capped by idle excess, not the 5 ETH shortfall)", d.Amount, want)
	}
}

// TestIdleExcessReservesPendingRedemptions guards the subtraction that keeps
// the keeper from depositing ETH that queued redemptions have already claimed.
func TestIdleExcessReservesPendingRedemptions(t *testing.T) {
	s := idleState()
	s.ControllerBalance = eth(10)
	s.NeedsETH = eth(9) // 1 ETH reserve + 9 ETH needed = nothing spare
	s.AMMFreeBalance = eth(5)

	d, err := strategy.Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action == strategy.ActionDepositExcess {
		t.Errorf("proposed DepositExcess of %s wei, but pending redemptions have claimed the balance", d.Amount)
	}
}

func TestDecideAmountIsDiagnosticOnly(t *testing.T) {
	s := idleState()
	s.ControllerBalance = eth(1)
	s.NeedsETH = eth(50)

	d, err := strategy.Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != strategy.ActionWithdrawShortfall {
		t.Fatalf("Action = %s, want WithdrawShortfall", d.Action)
	}

	// The amount is computed for the log, but must not reach the report.
	report, err := strategy.Report{ChainSelector: sepolia, Sequence: 1, ObservedAt: s.Now}.Build(d.Action)
	if err != nil {
		t.Fatal(err)
	}
	if err := strategy.ValidateParams(nil); err != nil {
		t.Fatal(err)
	}
	// 7 words: tuple offset, 3 header fields, action, params offset, params length.
	if len(report) != 7*32 {
		t.Errorf("report is %d bytes; an action-only W2 report is 7 words", len(report))
	}
}
