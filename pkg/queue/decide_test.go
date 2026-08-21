package queue_test

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/everstrat-xyz/keepers/pkg/queue"
)

const (
	oneETH  = 1_000_000_000_000_000_000
	nowTS   = uint64(1_700_000_000)
	dayS    = uint64(86_400)
	threeDS = 3 * dayS
)

func eth(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(oneETH))
}

func wei(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad wei literal " + s)
	}
	return v
}

func user(n byte) common.Address {
	var a common.Address
	a[19] = n
	return a
}

// req builds a request that costs exactly `tokens` ETH at a price of 1 ETH per
// EVE, with no slippage closure.
func req(n byte, tokens int64) queue.Request {
	return queue.Request{
		User:                  user(n),
		EvePriceAtRequestTime: eth(1),
		TokensToBurn:          eth(tokens),
		PriceTolerance:        big.NewInt(0),
	}
}

// batch builds a priced, processable batch from a request list.
func batch(id uint64, reqs ...queue.Request) queue.Batch {
	return queue.Batch{
		ID:               id,
		CanBeProcessed:   true,
		FinalEvePrice:    eth(1),
		CreatedAt:        nowTS - 2*dayS,
		PricedAt:         nowTS - dayS,
		UnprocessedCount: uint64(len(reqs)),
		Requests:         reqs,
	}
}

func stateWith(batches ...queue.Batch) queue.State {
	m := map[uint64]queue.Batch{}
	var maxID uint64
	for _, b := range batches {
		m[b.ID] = b
		if b.ID > maxID {
			maxID = b.ID
		}
	}
	return queue.State{
		Now:                    nowTS,
		CurrentBatchID:         maxID + 1,
		NextBatchIDToProcess:   1,
		ControllerBalance:      eth(1000),
		MaxBatchProcessingTime: threeDS,
		MinBatchAge:            dayS,
		MaxUsersPerUpkeep:      20,
		Batches:                m,
	}
}

func TestIsBatchSkippable(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*queue.Batch)
		now  uint64
		want bool
	}{
		{
			name: "fully processed batch is skippable",
			mut:  func(b *queue.Batch) { b.UnprocessedCount = 0 },
			want: true,
		},
		{
			name: "priced batch with work left is not skippable",
			mut:  func(*queue.Batch) {},
			want: false,
		},
		{
			name: "unpriced batch is not skippable",
			mut:  func(b *queue.Batch) { b.PricedAt = 0; b.CanBeProcessed = false },
			want: false,
		},
		{
			// canBeProcessed false short-circuits before the expiry check.
			name: "not-yet-processable batch is not skippable even when old",
			mut:  func(b *queue.Batch) { b.CanBeProcessed = false; b.PricedAt = nowTS - 10*dayS },
			want: false,
		},
		{
			// Contracts PR #43 reordered the guard: an unpriced batch is never
			// skippable even when empty, because it can still receive requests
			// and must be priced first.
			name: "unpriced empty batch is not skippable",
			mut:  func(b *queue.Batch) { b.CanBeProcessed = false; b.PricedAt = 0; b.UnprocessedCount = 0 },
			want: false,
		},
		{
			name: "expired batch past MAX_BATCH_PROCESSING_TIME is skippable",
			mut:  func(b *queue.Batch) { b.PricedAt = nowTS - threeDS - 1 },
			want: true,
		},
		{
			// The contract uses a strict `>`, so exactly at the boundary the
			// batch is still live.
			name: "batch exactly at the expiry boundary is not skippable",
			mut:  func(b *queue.Batch) { b.PricedAt = nowTS - threeDS },
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := batch(1, req(1, 1))
			tt.mut(&b)
			s := stateWith(b)
			if got := s.IsBatchSkippable(1); got != tt.want {
				t.Errorf("IsBatchSkippable() = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("unreadable batch is not skippable", func(t *testing.T) {
		s := stateWith(batch(1, req(1, 1)))
		if s.IsBatchSkippable(99) {
			t.Error("IsBatchSkippable(unknown) = true; skipping an unreadable batch would advance the cursor over live work")
		}
	})
}

func TestAffordableRequests(t *testing.T) {
	tests := []struct {
		name    string
		batch   queue.Batch
		balance *big.Int
		maxUsrs uint64
		want    uint64
	}{
		{
			name:    "all requests fit",
			batch:   batch(1, req(1, 1), req(2, 1), req(3, 1)),
			balance: eth(10),
			want:    3,
		},
		{
			name:    "balance covers a prefix only",
			batch:   batch(1, req(1, 1), req(2, 1), req(3, 1)),
			balance: eth(2),
			want:    2,
		},
		{
			// The contract breaks at the first unaffordable request rather
			// than skipping it to fit cheaper ones behind it.
			name:    "stops at the first unaffordable request without skipping",
			batch:   batch(1, req(1, 1), req(2, 100), req(3, 1)),
			balance: eth(5),
			want:    1,
		},
		{
			name:    "exact balance fits the whole prefix",
			batch:   batch(1, req(1, 1), req(2, 1)),
			balance: eth(2),
			want:    2,
		},
		{
			name:    "one wei short drops the last request",
			batch:   batch(1, req(1, 1), req(2, 1)),
			balance: new(big.Int).Sub(eth(2), big.NewInt(1)),
			want:    1,
		},
		{
			name:    "zero balance affords nothing",
			batch:   batch(1, req(1, 1)),
			balance: new(big.Int),
			want:    0,
		},
		{
			name:    "capped at maxUsersPerUpkeep",
			batch:   batch(1, req(1, 1), req(2, 1), req(3, 1), req(4, 1)),
			balance: eth(1000),
			maxUsrs: 2,
			want:    2,
		},
		{
			name: "unpriced batch affords nothing",
			batch: func() queue.Batch {
				b := batch(1, req(1, 1))
				b.CanBeProcessed = false
				return b
			}(),
			balance: eth(1000),
			want:    0,
		},
		{
			name: "empty batch affords nothing",
			batch: func() queue.Batch {
				b := batch(1)
				b.UnprocessedCount = 0
				return b
			}(),
			balance: eth(1000),
			want:    0,
		},
		{
			// Contracts PR #43: a batch past its processing window returns
			// zero on both sides — `pullRequest` reverts `ExitQueueBatchExpired`,
			// so no balance, however large, can settle it.
			name: "expired batch affords nothing regardless of balance",
			batch: func() queue.Batch {
				b := batch(1, req(1, 1))
				b.PricedAt = nowTS - threeDS - 1
				return b
			}(),
			balance: eth(1000),
			want:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := stateWith(tt.batch)
			s.ControllerBalance = tt.balance
			if tt.maxUsrs > 0 {
				s.MaxUsersPerUpkeep = tt.maxUsrs
			}
			got, err := s.AffordableRequests(1)
			if err != nil {
				t.Fatalf("AffordableRequests() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("AffordableRequests() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestAffordableRequestsZeroCostSlippage covers the request that is closed at
// zero ETH because the batch priced more than the user's tolerance below their
// queued price. It consumes a slot but no budget, so it must not stop the walk.
func TestAffordableRequestsZeroCostSlippage(t *testing.T) {
	slipped := queue.Request{
		User:                  user(2),
		EvePriceAtRequestTime: eth(1),
		TokensToBurn:          eth(1000), // would be unaffordable if it cost anything
		PriceTolerance:        wei("50000000000000000"),
	}
	b := batch(1, req(1, 1), slipped, req(3, 1))
	b.FinalEvePrice = wei("900000000000000000") // 10% below, past the 5% tolerance

	s := stateWith(b)
	s.ControllerBalance = eth(3)

	got, err := s.AffordableRequests(1)
	if err != nil {
		t.Fatalf("AffordableRequests() error = %v", err)
	}
	if got != 3 {
		t.Errorf("AffordableRequests() = %d, want 3 (the slipped request costs nothing and must not stop the walk)", got)
	}
}

func TestAffordableRequestsRejectsBadTolerance(t *testing.T) {
	bad := req(1, 1)
	bad.PriceTolerance = new(big.Int).Add(wei("1000000000000000000"), big.NewInt(1))
	s := stateWith(batch(1, bad))

	if _, err := s.AffordableRequests(1); err == nil {
		t.Error("AffordableRequests() succeeded on a tolerance the contract would revert on")
	}
}

func TestPeekAdvancedCursor(t *testing.T) {
	done := func(id uint64) queue.Batch {
		b := batch(id, req(1, 1))
		b.UnprocessedCount = 0
		return b
	}

	t.Run("walks past processed batches", func(t *testing.T) {
		s := stateWith(done(1), done(2), batch(3, req(1, 1)))
		if got := s.PeekAdvancedCursor(0); got != 3 {
			t.Errorf("PeekAdvancedCursor(unbounded) = %d, want 3", got)
		}
	})

	t.Run("stops at live work", func(t *testing.T) {
		s := stateWith(done(1), batch(2, req(1, 1)), done(3))
		if got := s.PeekAdvancedCursor(0); got != 2 {
			t.Errorf("PeekAdvancedCursor() = %d, want 2", got)
		}
	})

	t.Run("never passes the current batch", func(t *testing.T) {
		s := stateWith(done(1), done(2))
		// CurrentBatchID is 3 here; the cursor must stop there even though
		// batch 2 is skippable.
		if got := s.PeekAdvancedCursor(0); got != 3 {
			t.Errorf("PeekAdvancedCursor() = %d, want 3 (the current batch)", got)
		}
	})

	// The bounded walk is what the receiver performs, so it decides how far an
	// AdvanceCursor claim can reach.
	t.Run("bounded walk stops after MaxBatchScan", func(t *testing.T) {
		var batches []queue.Batch
		for id := uint64(1); id <= 60; id++ {
			batches = append(batches, done(id))
		}
		batches = append(batches, batch(61, req(1, 1)))
		s := stateWith(batches...)

		if got, want := s.PeekAdvancedCursor(queue.MaxBatchScan), uint64(1+queue.MaxBatchScan); got != want {
			t.Errorf("PeekAdvancedCursor(MaxBatchScan) = %d, want %d", got, want)
		}
		if got := s.PeekAdvancedCursor(0); got != 61 {
			t.Errorf("PeekAdvancedCursor(unbounded) = %d, want 61", got)
		}
	})
}

func TestDecidePriorityOrder(t *testing.T) {
	t.Run("paused short-circuits everything", func(t *testing.T) {
		s := stateWith(batch(1, req(1, 1)))
		s.Paused = true
		d, err := queue.Decide(s)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != queue.ActionNone {
			t.Errorf("Action = %s, want None while paused", d.Action)
		}
	})

	t.Run("processing beats pricing", func(t *testing.T) {
		// Batch 1 has affordable work; batch 2 is the current batch and is old
		// enough to price.
		current := batch(2, req(5, 1))
		current.CanBeProcessed = false
		current.PricedAt = 0
		current.CreatedAt = nowTS - 2*dayS

		s := stateWith(batch(1, req(1, 1)), current)
		s.CurrentBatchID = 2

		d, err := queue.Decide(s)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != queue.ActionProcessRequests || d.BatchID != 1 || d.EndIndex != 1 {
			t.Errorf("got %s batch %d end %d, want ProcessRequests batch 1 end 1", d.Action, d.BatchID, d.EndIndex)
		}
	})

	t.Run("prices the current batch when nothing is processable", func(t *testing.T) {
		current := batch(1, req(1, 1))
		current.CanBeProcessed = false
		current.PricedAt = 0
		current.CreatedAt = nowTS - 2*dayS

		s := stateWith(current)
		s.CurrentBatchID = 1

		d, err := queue.Decide(s)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != queue.ActionPriceBatch || d.BatchID != 1 {
			t.Errorf("got %s batch %d, want PriceBatch batch 1", d.Action, d.BatchID)
		}
	})

	t.Run("refuses PriceBatch when the process walk was truncated", func(t *testing.T) {
		// Batch 1 looks like it might have work, but users were not loaded.
		// Pricing batch 2 would be accepted on-chain even if batch 1 was
		// processable — grow live-priced instead of settle.
		head := batch(1, req(1, 1))
		head.Requests = nil
		current := batch(2, req(2, 1))
		current.CanBeProcessed = false
		current.PricedAt = 0
		current.CreatedAt = nowTS - 2*dayS

		s := stateWith(head, current)
		s.CurrentBatchID = 2
		s.ScanTruncatedAt = 1

		d, err := queue.Decide(s)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action == queue.ActionPriceBatch {
			t.Fatalf("Action = PriceBatch; a truncated process walk must not price")
		}
		if d.Action != queue.ActionNone {
			t.Errorf("got %s, want None (AdvanceCursor is not due; batch 1 is not skippable)", d.Action)
		}
	})

	t.Run("still processes when truncated if an affordable prefix was loaded", func(t *testing.T) {
		head := batch(1, req(1, 1))
		s := stateWith(head)
		s.ScanTruncatedAt = 1

		d, err := queue.Decide(s)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != queue.ActionProcessRequests || d.BatchID != 1 {
			t.Errorf("got %s batch %d, want ProcessRequests batch 1", d.Action, d.BatchID)
		}
	})

	t.Run("still advances the cursor when truncated", func(t *testing.T) {
		done := batch(1, req(1, 1))
		done.UnprocessedCount = 0
		current := batch(2)
		current.UnprocessedCount = 0
		current.CanBeProcessed = false
		current.CreatedAt = nowTS

		s := stateWith(done, current)
		s.CurrentBatchID = 2
		s.ScanTruncatedAt = 1

		d, err := queue.Decide(s)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != queue.ActionAdvanceCursor || d.BatchID != 2 {
			t.Errorf("got %s batch %d, want AdvanceCursor to 2", d.Action, d.BatchID)
		}
	})

	t.Run("does not price a batch younger than minBatchAge", func(t *testing.T) {
		current := batch(1, req(1, 1))
		current.CanBeProcessed = false
		current.PricedAt = 0
		current.CreatedAt = nowTS - dayS + 1 // one second short

		s := stateWith(current)
		s.CurrentBatchID = 1

		d, err := queue.Decide(s)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != queue.ActionNone {
			t.Errorf("Action = %s, want None for a batch below minBatchAge", d.Action)
		}
	})

	t.Run("advances the cursor when there is nothing else", func(t *testing.T) {
		done := batch(1, req(1, 1))
		done.UnprocessedCount = 0
		empty := batch(2)
		empty.UnprocessedCount = 0
		empty.CanBeProcessed = false
		empty.CreatedAt = nowTS

		s := stateWith(done, empty)
		s.CurrentBatchID = 2

		d, err := queue.Decide(s)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != queue.ActionAdvanceCursor || d.BatchID != 2 {
			t.Errorf("got %s batch %d, want AdvanceCursor to 2", d.Action, d.BatchID)
		}
	})

	t.Run("nothing to do", func(t *testing.T) {
		empty := batch(1)
		empty.UnprocessedCount = 0
		empty.CanBeProcessed = false
		empty.CreatedAt = nowTS

		s := stateWith(empty)
		s.CurrentBatchID = 1
		s.NextBatchIDToProcess = 1

		d, err := queue.Decide(s)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != queue.ActionNone {
			t.Errorf("Action = %s, want None", d.Action)
		}
	})
}

// TestDecideFindsWorkBeyondTheScanWindow is the full-scan improvement W1 exists
// to deliver: a processable batch past MaxBatchScan that queueUpkeepStatus
// structurally cannot reach. `_processReport` does not apply the window when
// validating a ProcessRequests claim, so the receiver still accepts it.
func TestDecideFindsWorkBeyondTheScanWindow(t *testing.T) {
	// The view's reach is two windows deep: it walks its bounded cursor to
	// 1+25=26, then scans [26, 51). So the target has to sit at 51 or beyond
	// to be genuinely invisible to it.
	var batches []queue.Batch
	for id := uint64(1); id <= 60; id++ {
		b := batch(id, req(1, 1))
		b.UnprocessedCount = 0 // processed, so the cursor walks past
		batches = append(batches, b)
	}
	batches = append(batches, batch(61, req(9, 1)))

	s := stateWith(batches...)
	s.CurrentBatchID = 62

	if got, want := s.OnChainScanWindowEnd(), uint64(51); got != want {
		t.Fatalf("OnChainScanWindowEnd() = %d, want %d", got, want)
	}

	d, err := queue.Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != queue.ActionProcessRequests || d.BatchID != 61 {
		t.Fatalf("got %s batch %d, want ProcessRequests batch 61", d.Action, d.BatchID)
	}
	if !d.ScannedBeyondWindow {
		t.Error("ScannedBeyondWindow = false; batch 61 is past the view's reach and the flag drives divergence classification")
	}
}

// TestDecideDoesNotOverclaimImprovement is the other side of the same
// boundary: a batch the gas-bounded view *can* reach must not be labelled a
// full-scan win, or the shadow window would be full of false improvements
// hiding real bugs.
func TestDecideDoesNotOverclaimImprovement(t *testing.T) {
	var batches []queue.Batch
	for id := uint64(1); id <= 40; id++ {
		b := batch(id, req(1, 1))
		b.UnprocessedCount = 0
		batches = append(batches, b)
	}
	batches = append(batches, batch(41, req(9, 1)))

	s := stateWith(batches...)
	s.CurrentBatchID = 42

	d, err := queue.Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if d.BatchID != 41 {
		t.Fatalf("BatchID = %d, want 41", d.BatchID)
	}
	if d.ScannedBeyondWindow {
		t.Error("ScannedBeyondWindow = true, but batch 41 falls inside the view's [26, 51) scan range")
	}
}

// TestDecideCapsAdvanceCursorAtReceiverReach is the constraint that keeps the
// full scan from producing a revert storm: `_processReport` advances the cursor
// with the *bounded* walk and reverts unless it reaches the claim, so W1 must
// not claim the unbounded cursor.
func TestDecideCapsAdvanceCursorAtReceiverReach(t *testing.T) {
	var batches []queue.Batch
	for id := uint64(1); id <= 60; id++ {
		b := batch(id, req(1, 1))
		b.UnprocessedCount = 0
		batches = append(batches, b)
	}
	// Current batch is empty and brand new, so neither processing nor pricing
	// applies and the decision falls through to AdvanceCursor.
	current := batch(61)
	current.UnprocessedCount = 0
	current.CanBeProcessed = false
	current.CreatedAt = nowTS
	batches = append(batches, current)

	s := stateWith(batches...)
	s.CurrentBatchID = 61

	d, err := queue.Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != queue.ActionAdvanceCursor {
		t.Fatalf("Action = %s, want AdvanceCursor", d.Action)
	}
	if want := uint64(1 + queue.MaxBatchScan); d.BatchID != want {
		t.Errorf("AdvanceCursor claim = %d, want %d — the receiver's bounded walk cannot reach further in one report",
			d.BatchID, want)
	}
}

// TestDecideOutputEncodesToAValidReport ties the decision engine to the wire
// format: whatever Decide proposes must survive the params encoders, which
// enforce the receiver's own constraints.
func TestDecideOutputEncodesToAValidReport(t *testing.T) {
	s := stateWith(batch(1, req(1, 1), req(2, 1)))
	d, err := queue.Decide(s)
	if err != nil {
		t.Fatal(err)
	}

	r := queue.Report{ChainSelector: sepolia, Sequence: 1, ObservedAt: s.Now}
	var report []byte
	switch d.Action {
	case queue.ActionProcessRequests:
		report, err = r.ProcessRequests(d.BatchID, d.EndIndex)
	case queue.ActionPriceBatch:
		report, err = r.PriceBatch(d.BatchID)
	case queue.ActionAdvanceCursor:
		report, err = r.AdvanceCursor(d.BatchID)
	default:
		t.Fatalf("unexpected action %s", d.Action)
	}
	if err != nil {
		t.Fatalf("encoding %s: %v", d.Action, err)
	}
	if len(report) == 0 {
		t.Error("encoded report is empty")
	}
}
