package strategy

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"github.com/everstrat-xyz/keepers/pkg/solmath"
)

// Scan bounds from CREStrategyExecutor. W2 mirrors them deliberately — see
// Decide.
const (
	// MaxBatchScan is CREStrategyExecutor.MAX_BATCH_SCAN.
	MaxBatchScan = 25
	// MaxUsersCostScan is CREStrategyExecutor.MAX_USERS_COST_SCAN.
	MaxUsersCostScan = 50
)

// Strategy is one registered strategy as read off-chain.
type Strategy struct {
	Address common.Address
	Paused  bool
	Healthy bool
	// MaxDeposit and MaxWithdrawal are capacity, not balances.
	MaxDeposit    *big.Int
	MaxWithdrawal *big.Int
	// InDepositCooldown comes from StrategyManager, not the strategy.
	InDepositCooldown bool
	// DepositWeight comes from StrategyManager. A registered-but-unfunded
	// strategy has weight 0 and can never take a deposit, so
	// `_depositCapacityAvailable` requires it to be non-zero (contracts PR
	// #43, R4-M-04) — without the gate W2 would recommend a DepositExcess the
	// StrategyManager always no-ops.
	DepositWeight uint8
	// PendingPerformanceFeeETH is this strategy's accrued fee.
	PendingPerformanceFeeETH *big.Int
}

// State is the snapshot W2 decides from.
type State struct {
	// Now is the observed block's timestamp — the same clock lastSyncAt was
	// recorded with. See docs/envelope.md.
	Now uint64

	// Paused is true when the receiver or Controller/StrategyManager is
	// paused; both `strategyUpkeepStatus` and every action gate on it.
	Paused bool

	ControllerBalance *big.Int

	// NeedsETH is the pending redemption cost, computed with the *same*
	// bounded scan the contract uses. See PendingRedemptionNeedsETH.
	NeedsETH *big.Int

	// AMMFreeBalance is the exit-side float.
	AMMFreeBalance *big.Int

	Strategies []Strategy

	// PerformanceFeeBps is zero when fees are disabled, which short-circuits
	// the harvest branch.
	PerformanceFeeBps *big.Int

	// Thresholds, mirroring the receiver's storage.
	ControllerReserveETH     *big.Int
	MinDepositETH            *big.Int
	MinWithdrawETH           *big.Int
	MinHarvestETH            *big.Int
	ExitLiquidityTargetETH   *big.Int
	MinExitLiquidityTopUpETH *big.Int
	SyncInterval             uint64
	LastSyncAt               uint64

	// ScanTruncated records that the read budget could not cover the full
	// bounded scan, so NeedsETH may understate the true figure.
	ScanTruncated bool
}

// Decision is the action W2 proposes.
//
// Amount is diagnostic only — it is logged, never reported. The receiver
// recomputes every quantity at execution time, which is why the report carries
// no amount at all.
type Decision struct {
	Action Action
	Amount *big.Int
	Reason string
}

// Decide selects the action, mirroring `CREStrategyExecutor.strategyUpkeepStatus`
// branch for branch and in the same order.
//
// # Why this deliberately does not compute a more exact shortfall
//
// Issue #4 framed W2's advantage as an "exact shortfall vs truncated on-chain
// scan". That would be actively harmful here, and the reason is worth stating:
// `_processReport` re-derives every quantity with the *same* bounded helpers
// the view uses (MAX_BATCH_SCAN batches, MAX_USERS_COST_SCAN users). A workflow
// that computed a truer shortfall would propose `WithdrawShortfall` in states
// where the receiver's own recomputation sees no shortfall — and revert with
// `KeeperExecutorNoUpkeepNeeded` every time.
//
// W1 is the opposite case: `_processReport` validates a `ProcessRequests` claim
// per batch, with no window, so scanning deeper than the view genuinely wins.
// The asymmetry is in the contracts, not in the workflows.
//
// So W2's value is not precision. It is running the decision against live state
// off-chain, cross-checking the view, and being the thing that can actually
// deliver a report.
func Decide(s State) (Decision, error) {
	zero := new(big.Int)

	if s.Paused {
		return Decision{Action: ActionNone, Amount: zero, Reason: "receiver or a protocol contract is paused"}, nil
	}

	// 1. Rebalance — any live strategy that is unhealthy.
	for _, st := range s.Strategies {
		if !st.Paused && !st.Healthy {
			return Decision{
				Action: ActionRebalance,
				Amount: zero,
				Reason: fmt.Sprintf("strategy %s is unhealthy and not paused", st.Address),
			}, nil
		}
	}

	needs := orZero(s.NeedsETH)
	balance := orZero(s.ControllerBalance)

	// 2. WithdrawShortfall — pending redemptions exceed the Controller's
	// balance by at least minWithdrawETH, and some strategy can actually pay.
	if needs.Cmp(balance) > 0 {
		shortfall := new(big.Int).Sub(needs, balance)
		if shortfall.Cmp(orZero(s.MinWithdrawETH)) >= 0 && s.totalMaxWithdrawal().Sign() > 0 {
			return Decision{
				Action: ActionWithdrawShortfall,
				Amount: shortfall,
				Reason: fmt.Sprintf("pending redemptions need %s wei against a controller balance of %s wei",
					needs, balance),
			}, nil
		}
	}

	// 3. ProvideExitLiquidity — top the AMM float back toward its target.
	topUp := s.exitLiquidityTopUp(balance, needs)
	if topUp.Cmp(orZero(s.MinExitLiquidityTopUpETH)) >= 0 && topUp.Sign() > 0 {
		return Decision{
			Action: ActionProvideExitLiquidity,
			Amount: topUp,
			Reason: fmt.Sprintf("AMM float %s wei is below the %s wei target; topping up by %s wei",
				orZero(s.AMMFreeBalance), orZero(s.ExitLiquidityTargetETH), topUp),
		}, nil
	}

	// 4. DepositExcess — idle ETH above the reserve, with somewhere to put it.
	excess := s.idleExcess(balance, needs)
	if excess.Cmp(orZero(s.MinDepositETH)) >= 0 && s.depositCapacityAvailable() {
		return Decision{
			Action: ActionDepositExcess,
			Amount: excess,
			Reason: fmt.Sprintf("controller holds %s wei idle above the reserve and pending redemptions", excess),
		}, nil
	}

	// 5. HarvestPerformanceFees.
	fee := s.pendingPerformanceFeeETH()
	if fee.Cmp(orZero(s.MinHarvestETH)) >= 0 && fee.Sign() > 0 {
		return Decision{
			Action: ActionHarvestPerformanceFees,
			Amount: fee,
			Reason: fmt.Sprintf("accrued performance fees are %s wei", fee),
		}, nil
	}

	// 6. Sync — periodic accounting refresh.
	if s.SyncInterval != 0 && len(s.Strategies) > 0 && s.Now >= s.LastSyncAt {
		if elapsed := s.Now - s.LastSyncAt; elapsed >= s.SyncInterval {
			return Decision{
				Action: ActionSync,
				Amount: zero,
				Reason: fmt.Sprintf("%ds since the last sync (interval %ds)", elapsed, s.SyncInterval),
			}, nil
		}
	}

	return Decision{Action: ActionNone, Amount: zero, Reason: "no strategy action is due"}, nil
}

// totalMaxWithdrawal mirrors _totalMaxWithdrawal.
func (s State) totalMaxWithdrawal() *big.Int {
	total := new(big.Int)
	for _, st := range s.Strategies {
		total.Add(total, orZero(st.MaxWithdrawal))
	}
	return total
}

// depositCapacityAvailable mirrors _depositCapacityAvailable.
//
// depositWeight > 0 is the R4-M-04 gate: all-zero weights are
// registered-but-unfunded — the StrategyManager skips them and refunds the
// Controller, so proposing DepositExcess against only those is work the
// receiver reverts on (the view says none is available).
func (s State) depositCapacityAvailable() bool {
	for _, st := range s.Strategies {
		if !st.InDepositCooldown && st.Healthy &&
			orZero(st.MaxDeposit).Sign() > 0 && st.DepositWeight > 0 {
			return true
		}
	}
	return false
}

// pendingPerformanceFeeETH mirrors _pendingPerformanceFeeETH, including the
// short-circuit when fees are disabled.
func (s State) pendingPerformanceFeeETH() *big.Int {
	total := new(big.Int)
	if orZero(s.PerformanceFeeBps).Sign() == 0 {
		return total
	}
	for _, st := range s.Strategies {
		total.Add(total, orZero(st.PendingPerformanceFeeETH))
	}
	return total
}

// idleExcess mirrors _idleExcess: balance above the reserve plus what pending
// redemptions have already claimed.
func (s State) idleExcess(balance, needs *big.Int) *big.Int {
	reserved := new(big.Int).Add(orZero(s.ControllerReserveETH), needs)
	if balance.Cmp(reserved) <= 0 {
		return new(big.Int)
	}
	return new(big.Int).Sub(balance, reserved)
}

// exitLiquidityTopUp mirrors _exitLiquidityTopUp: the smaller of the float
// shortfall and the idle excess.
func (s State) exitLiquidityTopUp(balance, needs *big.Int) *big.Int {
	target := orZero(s.ExitLiquidityTargetETH)
	if target.Sign() == 0 {
		return new(big.Int)
	}
	float := orZero(s.AMMFreeBalance)
	if float.Cmp(target) >= 0 {
		return new(big.Int)
	}

	shortfall := new(big.Int).Sub(target, float)
	excess := s.idleExcess(balance, needs)
	if shortfall.Cmp(excess) < 0 {
		return shortfall
	}
	return excess
}

// QueueBatch is the subset of a batch needed to price pending redemptions.
//
// TotalTokensToBurn is deliberately absent: the current batch's unpriced EVE is
// not a liability until `priceBatch` (contracts PR #43, M-11), so W2 never
// needs to read it.
type QueueBatch struct {
	ID               uint64
	CanBeProcessed   bool
	FinalEvePrice    *big.Int
	PricedAt         uint64
	UnprocessedCount uint64
	Requests         []QueueRequest
}

// QueueRequest is one unprocessed redemption request.
type QueueRequest struct {
	EvePriceAtRequestTime *big.Int
	TokensToBurn          *big.Int
	PriceTolerance        *big.Int
}

// PendingRedemptionNeedsETH mirrors `_pendingRedemptionNeedsETH`.
//
// cursor is the queue keeper's `nextLiveBatchIdToProcess()`. The walk is capped
// at MaxBatchScan batches and MaxUsersCostScan users per batch — matching the
// contract exactly, because the receiver re-derives this figure the same way
// when it validates the report. Widening the caps here would produce reports
// the receiver rejects.
//
// The current batch is deliberately absent: until `priceBatch` it is still
// cancellable equity (`liveRedemptionOffsets` is zero), so the contract counts
// it as no liability at all. Sizing it at the live base price here would make
// the workflow propose WithdrawShortfall in states where the receiver's own
// recomputation sees none — and every report would revert with
// KeeperExecutorNoUpkeepNeeded.
func PendingRedemptionNeedsETH(
	batches map[uint64]QueueBatch,
	cursor, currentBatchID uint64,
	maxBatchProcessingTime, now uint64,
) (*big.Int, error) {
	needs := new(big.Int)

	limit := cursor + MaxBatchScan
	for id := cursor; id < currentBatchID && id < limit; id++ {
		b, ok := batches[id]
		if !ok {
			continue
		}
		cost, err := batchSettlementCost(b, maxBatchProcessingTime, now)
		if err != nil {
			return nil, err
		}
		needs.Add(needs, cost)
	}
	return needs, nil
}

// batchSettlementCost mirrors _batchSettlementCost.
//
// Note the difference from W1's affordability walk: this sums every request's
// cost with no balance budget and no early break, because it is asking "what do
// pending redemptions cost?" rather than "what can we afford right now?".
func batchSettlementCost(b QueueBatch, maxBatchProcessingTime, now uint64) (*big.Int, error) {
	cost := new(big.Int)
	if !b.CanBeProcessed {
		return cost, nil
	}
	// A batch past the escape hatch is users' to close, not the keeper's.
	if b.PricedAt > 0 && now > b.PricedAt+maxBatchProcessingTime {
		return cost, nil
	}
	if b.UnprocessedCount == 0 {
		return cost, nil
	}

	limit := b.UnprocessedCount
	if limit > MaxUsersCostScan {
		limit = MaxUsersCostScan
	}
	if uint64(len(b.Requests)) < limit {
		limit = uint64(len(b.Requests))
	}

	for i := uint64(0); i < limit; i++ {
		r := b.Requests[i]
		slipped, err := solmath.IsRelativelyLessThan(
			orZero(b.FinalEvePrice), orZero(r.EvePriceAtRequestTime), orZero(r.PriceTolerance))
		if err != nil {
			return nil, fmt.Errorf("batch %d request %d: %w", b.ID, i, err)
		}
		if slipped {
			continue
		}
		cost.Add(cost, solmath.ConvertAssets(orZero(r.TokensToBurn), orZero(b.FinalEvePrice)))
	}
	return cost, nil
}

func orZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}
