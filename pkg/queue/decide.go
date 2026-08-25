package queue

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"github.com/everstrat-xyz/keepers/pkg/solmath"
)

// MaxBatchScan is CREQueueExecutor.MAX_BATCH_SCAN — the gas bound on the
// on-chain `queueUpkeepStatus` view and on `_advanceBatchCursor`.
//
// W1 deliberately scans past it (see Decide). The receiver's `_processReport`
// does *not* apply the window when validating a `ProcessRequests` claim, so a
// batch found beyond it is still executable — that is the whole point of
// scanning off-chain. `AdvanceCursor` is the exception, and Decide caps that
// claim accordingly.
const MaxBatchScan = 25

// Request is one queued redemption, as read from `ExitQueue.requestInfo`.
//
// Note what is absent: no ETH amount. The cost is derived from TokensToBurn and
// the batch's final price, exactly as the Controller derives it.
type Request struct {
	User                  common.Address
	Processed             bool
	ClosedDueToSlippage   bool
	EvePriceAtRequestTime *big.Int
	TokensToBurn          *big.Int
	PriceTolerance        *big.Int
}

// Batch is one ExitQueue batch, as read from `batchInfo` plus the unprocessed
// user list.
type Batch struct {
	ID                uint64
	CanBeProcessed    bool
	FinalEvePrice     *big.Int
	TotalTokensToBurn *big.Int
	CreatedAt         uint64
	PricedAt          uint64
	UnprocessedCount  uint64

	// Requests are the unprocessed requests in `unprocessedUsers` order,
	// starting at index 0. The receiver only accepts a prefix of this list, so
	// order is load-bearing, not incidental.
	//
	// It may be shorter than UnprocessedCount when the read was capped at
	// maxUsersPerUpkeep — which is all the affordability model needs, since the
	// receiver caps there too.
	Requests []Request
}

// State is the full off-chain snapshot W1 decides from. Every field is read
// on-chain in the same tick; nothing is remembered between ticks.
type State struct {
	// Now is the observation timestamp (consensus time), in unix seconds.
	Now uint64

	// Paused is true when the receiver or any of Controller / ExitQueue / AMM
	// is paused. The on-chain view returns None in that case and every action
	// would revert, so W1 must too.
	Paused bool

	CurrentBatchID uint64
	// NextBatchIDToProcess is the receiver's stored cursor.
	NextBatchIDToProcess uint64

	// ControllerBalance is the ETH budget redemptions are paid from. It is the
	// only balance affordability depends on — `Controller._processRequest`
	// sends from the Controller itself.
	ControllerBalance *big.Int

	MaxBatchProcessingTime uint64
	MinBatchAge            uint64
	MaxUsersPerUpkeep      uint64

	// Batches is the full scan, keyed by batch id. Missing entries are treated
	// as unreadable and skipped conservatively.
	Batches map[uint64]Batch

	// ScanTruncatedAt is the last batch id the read layer actually fetched when
	// it capped the scan. Zero means the scan reached the current batch.
	//
	// A truncated scan can only cause the workflow to propose *less* work than
	// exists, never wrong work — but it makes "the workflow found nothing"
	// ambiguous, so the tick logs it, Classify labels it truncated-scan, and
	// Decide refuses PriceBatch until a later tick can finish the process walk.
	ScanTruncatedAt uint64
}

// ScanTruncated reports whether the off-chain scan stopped short of the current
// batch.
func (s State) ScanTruncated() bool {
	return s.ScanTruncatedAt != 0 && s.ScanTruncatedAt < s.CurrentBatchID
}

// Decision is the action W1 proposes, with the reasoning that produced it.
//
// Reason is written for the shadow-mode log: when a divergence has to be
// triaged, "why did the workflow think this" is the expensive question.
type Decision struct {
	Action  Action
	BatchID uint64
	// EndIndex is the exclusive end of the claimed affordable prefix. Only
	// meaningful for ActionProcessRequests.
	EndIndex uint64
	Reason   string
	// ScannedBeyondWindow records that the chosen batch sits past
	// MaxBatchScan, i.e. the on-chain view structurally could not have found
	// it. Divergence classification uses this to separate a real improvement
	// from a bug.
	ScannedBeyondWindow bool
}

// Batch returns the batch with the given id.
func (s State) Batch(id uint64) (Batch, bool) {
	b, ok := s.Batches[id]
	return b, ok
}

// IsBatchSkippable mirrors CREQueueExecutor._isBatchSkippable.
//
// The unpriced guard comes first, so an unpriced batch — including the current
// one, even when still empty — is never skippable: it can still receive
// requests and must be priced first. A priced batch is skippable when it is
// fully processed, or when it has run past MAX_BATCH_PROCESSING_TIME (the
// escape hatch, after which users close their own requests and the keeper must
// not touch the batch).
//
// An unreadable batch is *not* skippable: skipping past a batch we could not
// read would advance the cursor over live work.
func (s State) IsBatchSkippable(id uint64) bool {
	b, ok := s.Batches[id]
	if !ok {
		return false
	}
	if !b.CanBeProcessed {
		return false
	}
	if b.UnprocessedCount == 0 {
		return true
	}
	return s.Now > b.PricedAt+s.MaxBatchProcessingTime
}

// NeedsUserScan reports whether this batch is one Decide might ProcessRequests,
// which cannot be known without requestInfo.
//
// Empty and expired batches are skippable — the view walks past them without
// reading users. Unpriced batches need PriceBatch, not a user list. A priced,
// in-window batch with leftover requests still needs users even if its first
// request later turns out to overrun the Controller balance: Decide (and
// queueUpkeepStatus) continue to the next such batch rather than stalling.
func (s State) NeedsUserScan(id uint64) bool {
	b, ok := s.Batches[id]
	if !ok || !b.CanBeProcessed || b.UnprocessedCount == 0 {
		return false
	}
	return !s.IsBatchSkippable(id)
}

// AffordableRequests mirrors CREQueueExecutor._affordableRequests: how many
// requests, taken as a prefix from index 0, the Controller's balance covers.
//
// The contract walks the prefix accumulating cost and stops at the first
// request that would overrun the balance — it does not skip an expensive
// request to fit cheaper ones behind it. Reproducing the break (rather than
// "fit as many as possible") is what keeps the claim acceptable to the
// receiver.
//
// A batch past its processing window returns zero on both sides: the receiver
// cannot settle it, and users recover via the escape hatch.
func (s State) AffordableRequests(id uint64) (uint64, error) {
	b, ok := s.Batches[id]
	if !ok {
		return 0, nil
	}
	if !b.CanBeProcessed {
		return 0, nil
	}
	// Priced but past the processing window: `pullRequest` would revert
	// `ExitQueueBatchExpired`, so the receiver's own walk returns zero here and
	// the batch is the users' to close, not the keeper's.
	if b.PricedAt > 0 && s.Now > b.PricedAt+s.MaxBatchProcessingTime {
		return 0, nil
	}
	if b.UnprocessedCount == 0 {
		return 0, nil
	}

	limit := b.UnprocessedCount
	if limit > s.MaxUsersPerUpkeep {
		limit = s.MaxUsersPerUpkeep
	}
	if uint64(len(b.Requests)) < limit {
		limit = uint64(len(b.Requests))
	}

	budget := s.ControllerBalance
	if budget == nil {
		budget = new(big.Int)
	}

	cumulative := new(big.Int)
	var count uint64
	for i := uint64(0); i < limit; i++ {
		cost, err := RequestCost(b.FinalEvePrice, b.Requests[i])
		if err != nil {
			// A tolerance the contract would revert on. Stop here rather than
			// guess: the prefix up to this point is still valid.
			return count, fmt.Errorf("batch %d request %d: %w", id, i, err)
		}
		next := new(big.Int).Add(cumulative, cost)
		if next.Cmp(budget) > 0 {
			break
		}
		cumulative = next
		count++
	}
	return count, nil
}

// RequestCost mirrors the ETH a single request costs the Controller, per
// `Controller._processRequest`.
//
// A request whose batch price fell more than the user's tolerance below their
// queued price is closed at zero cost — it consumes a slot but no ETH.
func RequestCost(finalEvePrice *big.Int, r Request) (*big.Int, error) {
	slipped, err := solmath.IsRelativelyLessThan(finalEvePrice, r.EvePriceAtRequestTime, r.PriceTolerance)
	if err != nil {
		return nil, err
	}
	if slipped {
		return new(big.Int), nil
	}
	return solmath.ConvertAssets(r.TokensToBurn, finalEvePrice), nil
}

// OnChainScanWindowEnd is the first batch id `queueUpkeepStatus` cannot reach.
//
// The view walks its own bounded cursor first, then scans at most MaxBatchScan
// batches from wherever that lands — so its reach is two windows deep from the
// stored cursor, not one. Both Decide and Classify go through this helper so
// the "did the view have a chance to see this?" question has exactly one
// answer.
func (s State) OnChainScanWindowEnd() uint64 {
	return s.PeekAdvancedCursor(MaxBatchScan) + MaxBatchScan
}

// PeekAdvancedCursor mirrors CREQueueExecutor._peekAdvancedCursor.
//
// scanLimit bounds how far the cursor may walk. Pass MaxBatchScan to reproduce
// what the receiver will do; pass 0 for an unbounded off-chain walk.
func (s State) PeekAdvancedCursor(scanLimit uint64) uint64 {
	cursor := s.NextBatchIDToProcess
	stop := s.CurrentBatchID
	if scanLimit > 0 && cursor+scanLimit < stop {
		stop = cursor + scanLimit
	}
	for cursor < stop && s.IsBatchSkippable(cursor) {
		cursor++
	}
	return cursor
}

// Decide selects the action W1 proposes, following the same priority order as
// `CREQueueExecutor.queueUpkeepStatus` — process work first, then price the
// current batch, then advance the cursor.
//
// The deliberate difference from the on-chain view is the scan width: this
// walks every batch from the cursor to the current one, where the view stops
// after MaxBatchScan. That is the improvement the off-chain keeper exists to
// provide, and it is safe for `ProcessRequests` because `_processReport`
// re-derives affordability for the named batch without applying the window.
//
// `AdvanceCursor` gets the opposite treatment: the receiver advances its cursor
// with the *bounded* walk, so a claim past `cursor + MaxBatchScan` is
// unreachable in one report and reverts. Decide therefore claims only the
// bounded cursor.
func Decide(s State) (Decision, error) {
	if s.Paused {
		return Decision{Action: ActionNone, Reason: "receiver or a protocol contract is paused"}, nil
	}

	fullCursor := s.PeekAdvancedCursor(0)
	windowEnd := s.OnChainScanWindowEnd()

	// Process the oldest batch with an affordable prefix. Full scan.
	for id := fullCursor; id < s.CurrentBatchID; id++ {
		if s.IsBatchSkippable(id) {
			continue
		}
		affordable, err := s.AffordableRequests(id)
		if err != nil {
			return Decision{}, err
		}
		if affordable == 0 {
			continue
		}
		beyond := id >= windowEnd
		return Decision{
			Action:              ActionProcessRequests,
			BatchID:             id,
			EndIndex:            affordable,
			ScannedBeyondWindow: beyond,
			Reason: fmt.Sprintf("batch %d has %d affordable requests within a controller balance of %s wei",
				id, affordable, s.ControllerBalance),
		}, nil
	}

	// PriceBatch is accepted even when ProcessRequests was also due, and would
	// grow the live-priced set instead of settling. If the process walk did not
	// finish, we cannot know that nothing was affordable — skip pricing this
	// tick. AdvanceCursor is still safe: it only walks skippable batches we
	// already have headers for.
	if !s.ScanTruncated() {
		if b, ok := s.Batches[s.CurrentBatchID]; ok && b.UnprocessedCount > 0 {
			if age := s.Now - b.CreatedAt; s.Now >= b.CreatedAt && age >= s.MinBatchAge {
				return Decision{
					Action:  ActionPriceBatch,
					BatchID: s.CurrentBatchID,
					Reason: fmt.Sprintf("current batch %d has %d unprocessed requests and is %ds old (minBatchAge %ds)",
						s.CurrentBatchID, b.UnprocessedCount, age, s.MinBatchAge),
				}, nil
			}
		}
	}

	// Advance the cursor past dead batches — capped at what the receiver can
	// actually reach in one report.
	if boundedCursor := s.PeekAdvancedCursor(MaxBatchScan); boundedCursor > s.NextBatchIDToProcess {
		return Decision{
			Action:  ActionAdvanceCursor,
			BatchID: boundedCursor,
			Reason: fmt.Sprintf("cursor can advance from %d to %d past fully-processed or expired batches",
				s.NextBatchIDToProcess, boundedCursor),
		}, nil
	}

	return Decision{Action: ActionNone, Reason: "no batch needs pricing, processing, or a cursor advance"}, nil
}
