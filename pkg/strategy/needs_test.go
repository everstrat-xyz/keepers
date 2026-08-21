package strategy_test

import (
	"math/big"
	"testing"

	"github.com/everstrat-xyz/keepers/pkg/strategy"
)

func wei(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad wei literal " + s)
	}
	return v
}

// request costs `tokens` ETH at a price of 1 ETH per EVE with no slippage.
func request(tokens int64) strategy.QueueRequest {
	return strategy.QueueRequest{
		EvePriceAtRequestTime: eth(1),
		TokensToBurn:          eth(tokens),
		PriceTolerance:        new(big.Int),
	}
}

func pricedBatch(id uint64, reqs ...strategy.QueueRequest) strategy.QueueBatch {
	return strategy.QueueBatch{
		ID:               id,
		CanBeProcessed:   true,
		FinalEvePrice:    eth(1),
		PricedAt:         1_700_000_000,
		UnprocessedCount: uint64(len(reqs)),
		Requests:         reqs,
	}
}

const (
	now       = uint64(1_700_000_100)
	maxProcTS = uint64(3 * 86400)
)

func TestPendingRedemptionNeedsETH(t *testing.T) {
	batches := map[uint64]strategy.QueueBatch{
		1: pricedBatch(1, request(1), request(2)),
		2: pricedBatch(2, request(3)),
	}
	// Batch 3 is the current one: unpriced, so it contributes nothing (M-11).
	batches[3] = strategy.QueueBatch{ID: 3}

	got, err := strategy.PendingRedemptionNeedsETH(batches, 1, 3, maxProcTS, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := eth(6); got.Cmp(want) != 0 { // 1+2+3 queued priced; the unpriced current batch adds 0
		t.Errorf("needs = %s, want %s", got, want)
	}
}

// TestUnpricedCurrentBatchIsNotALiability is the M-11 pin: until `priceBatch`,
// queued EVE is cancellable equity (`liveRedemptionOffsets` is zero), so
// counting it at the live base price would let a queue-then-cancel loop pull
// LP via WithdrawShortfall — and would propose shortfalls the receiver's own
// recomputation rejects.
func TestUnpricedCurrentBatchIsNotALiability(t *testing.T) {
	current := strategy.QueueBatch{
		ID:             2,
		CanBeProcessed: false, // unpriced
	}
	got, err := strategy.PendingRedemptionNeedsETH(
		map[uint64]strategy.QueueBatch{2: current}, 1, 2, maxProcTS, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := new(big.Int); got.Cmp(want) != 0 {
		t.Errorf("needs = %s, want 0 — the unpriced batch is not a keeper liability until priceBatch", got)
	}
}

// TestPricedBatchBecomesALiabilityWhenItEntersTheWindow covers the other half
// of the M-11 semantics: once `priceBatch` runs, the batch sits in
// [cursor, currentBatchId) and is costed at finalEvePrice.
func TestPricedBatchBecomesALiabilityWhenItEntersTheWindow(t *testing.T) {
	batches := map[uint64]strategy.QueueBatch{
		1: pricedBatch(1, request(5)),
	}
	// currentBatchId moved past it: batch 1 is now priced and in-window.
	got, err := strategy.PendingRedemptionNeedsETH(batches, 1, 2, maxProcTS, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := eth(5); got.Cmp(want) != 0 {
		t.Errorf("needs = %s, want %s — a priced in-window batch must be costed at finalEvePrice", got, want)
	}
}

// TestBatchSettlementCostSumsWithoutABudget is the difference from W1's
// affordability walk: this asks what redemptions cost, not what is affordable,
// so there is no balance budget and no early break.
func TestBatchSettlementCostSumsWithoutABudget(t *testing.T) {
	batches := map[uint64]strategy.QueueBatch{
		1: pricedBatch(1, request(1), request(1000), request(1)),
	}
	got, err := strategy.PendingRedemptionNeedsETH(batches, 1, 2, maxProcTS, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := eth(1002); got.Cmp(want) != 0 {
		t.Errorf("needs = %s, want %s (an expensive request must not stop the sum)", got, want)
	}
}

func TestBatchSettlementCostSkips(t *testing.T) {
	tests := []struct {
		name  string
		batch strategy.QueueBatch
		want  *big.Int
	}{
		{
			name: "unpriced batch contributes nothing",
			batch: func() strategy.QueueBatch {
				b := pricedBatch(1, request(5))
				b.CanBeProcessed = false
				return b
			}(),
			want: new(big.Int),
		},
		{
			name: "batch past the escape hatch contributes nothing",
			batch: func() strategy.QueueBatch {
				b := pricedBatch(1, request(5))
				b.PricedAt = now - maxProcTS - 1
				return b
			}(),
			want: new(big.Int),
		},
		{
			name: "batch exactly at the escape-hatch boundary still counts",
			batch: func() strategy.QueueBatch {
				b := pricedBatch(1, request(5))
				b.PricedAt = now - maxProcTS
				return b
			}(),
			want: eth(5),
		},
		{
			name: "fully processed batch contributes nothing",
			batch: func() strategy.QueueBatch {
				b := pricedBatch(1, request(5))
				b.UnprocessedCount = 0
				return b
			}(),
			want: new(big.Int),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := strategy.PendingRedemptionNeedsETH(
				map[uint64]strategy.QueueBatch{1: tt.batch}, 1, 2, maxProcTS, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Cmp(tt.want) != 0 {
				t.Errorf("needs = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNeedsCostScan(t *testing.T) {
	live := pricedBatch(1, request(1))
	if !live.NeedsCostScan(now, maxProcTS) {
		t.Error("live in-window batch: NeedsCostScan = false")
	}

	unpriced := pricedBatch(1, request(1))
	unpriced.CanBeProcessed = false
	if unpriced.NeedsCostScan(now, maxProcTS) {
		t.Error("unpriced batch: NeedsCostScan = true; no user list can change NeedsETH")
	}

	empty := pricedBatch(1, request(1))
	empty.UnprocessedCount = 0
	if empty.NeedsCostScan(now, maxProcTS) {
		t.Error("empty batch: NeedsCostScan = true")
	}

	expired := pricedBatch(1, request(1))
	expired.PricedAt = now - maxProcTS - 1
	if expired.NeedsCostScan(now, maxProcTS) {
		t.Error("expired batch: NeedsCostScan = true; _batchSettlementCost is 0 without users")
	}

	atBoundary := pricedBatch(1, request(1))
	atBoundary.PricedAt = now - maxProcTS
	if !atBoundary.NeedsCostScan(now, maxProcTS) {
		t.Error("exactly at escape-hatch boundary: NeedsCostScan = false; strict '>' still counts")
	}
}

// TestSlippedRequestsAreExcluded: a request whose batch priced more than the
// user's tolerance below their queued price settles at zero, so it costs the
// Controller nothing.
func TestSlippedRequestsAreExcluded(t *testing.T) {
	// Note the tolerances. `request()` uses a zero tolerance, which means *any*
	// drop closes it at zero — so the surviving request needs one wide enough
	// to cover the batch's 10% fall.
	tolerant := strategy.QueueRequest{
		EvePriceAtRequestTime: eth(1),
		TokensToBurn:          eth(1),
		PriceTolerance:        wei("200000000000000000"), // 20%
	}
	slipped := strategy.QueueRequest{
		EvePriceAtRequestTime: eth(1),
		TokensToBurn:          eth(1000),
		PriceTolerance:        wei("50000000000000000"), // 5%
	}
	b := pricedBatch(1, tolerant, slipped)
	b.FinalEvePrice = wei("900000000000000000") // 10% below

	got, err := strategy.PendingRedemptionNeedsETH(
		map[uint64]strategy.QueueBatch{1: b}, 1, 2, maxProcTS, now)
	if err != nil {
		t.Fatal(err)
	}
	// Only the tolerant request counts, priced at the batch's 0.9 ETH.
	if want := wei("900000000000000000"); got.Cmp(want) != 0 {
		t.Errorf("needs = %s, want %s", got, want)
	}
}

// TestScanCapsMatchTheContract is the load-bearing test for W2's design: the
// walk must stop where the contract stops. Scanning further would compute a
// truer figure and propose actions the receiver's own recomputation rejects.
func TestScanCapsMatchTheContract(t *testing.T) {
	if strategy.MaxBatchScan != 25 {
		t.Errorf("MaxBatchScan = %d, want 25 (CREStrategyExecutor.MAX_BATCH_SCAN)", strategy.MaxBatchScan)
	}
	if strategy.MaxUsersCostScan != 50 {
		t.Errorf("MaxUsersCostScan = %d, want 50 (CREStrategyExecutor.MAX_USERS_COST_SCAN)", strategy.MaxUsersCostScan)
	}

	t.Run("batch walk stops after MaxBatchScan", func(t *testing.T) {
		batches := map[uint64]strategy.QueueBatch{}
		for id := uint64(1); id <= 40; id++ {
			batches[id] = pricedBatch(id, request(1))
		}
		got, err := strategy.PendingRedemptionNeedsETH(batches, 1, 41, maxProcTS, now)
		if err != nil {
			t.Fatal(err)
		}
		if want := eth(strategy.MaxBatchScan); got.Cmp(want) != 0 {
			t.Errorf("needs = %s, want %s — a deeper walk than the contract's produces reports it rejects", got, want)
		}
	})

	t.Run("user walk stops after MaxUsersCostScan", func(t *testing.T) {
		var reqs []strategy.QueueRequest
		for i := 0; i < 80; i++ {
			reqs = append(reqs, request(1))
		}
		b := pricedBatch(1, reqs...)
		got, err := strategy.PendingRedemptionNeedsETH(
			map[uint64]strategy.QueueBatch{1: b}, 1, 2, maxProcTS, now)
		if err != nil {
			t.Fatal(err)
		}
		if want := eth(strategy.MaxUsersCostScan); got.Cmp(want) != 0 {
			t.Errorf("needs = %s, want %s", got, want)
		}
	})
}

func TestPendingRedemptionNeedsETHHandlesGaps(t *testing.T) {
	// A batch the read budget could not fetch is simply absent; the walk must
	// skip it rather than panic on a nil entry.
	batches := map[uint64]strategy.QueueBatch{
		1: pricedBatch(1, request(1)),
		3: pricedBatch(3, request(1)),
	}
	got, err := strategy.PendingRedemptionNeedsETH(batches, 1, 4, maxProcTS, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := eth(2); got.Cmp(want) != 0 {
		t.Errorf("needs = %s, want %s", got, want)
	}
}
