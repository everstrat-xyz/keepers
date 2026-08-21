//go:build wasip1

package main

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	everabi "github.com/everstrat-xyz/keepers/contracts/evm/src/abi"
	"github.com/everstrat-xyz/keepers/pkg/evmread"
	"github.com/everstrat-xyz/keepers/pkg/queue"
	"github.com/everstrat-xyz/keepers/pkg/registry"
)

// The read plan, against CRE's budget of 15 contract reads per execution
// (evmread.MaxChainReads):
//
//	1  multicall  registry lookups + receiver config + pause flags   (~13 sub-calls)
//	1  balance    Controller ETH
//	1  multicall  queueUpkeepStatus cross-check
//	N  multicall  batchInfo + unprocessedUsersCount per batch
//	M  multicall  unprocessedUsers per processable batch
//	K  multicall  requestInfo per unprocessed request
//
// Everything after the first two rounds is budgeted: the scan takes what is
// left and stops cleanly, marking the state truncated. A truncated scan can
// only under-propose work, never propose wrong work.
//
// Sizing note: batchInfo and requestInfo return 160 bytes each, so roughly 16
// results fit in the 5kb payload limit (evmread.ChunkSubCalls handles this).

// reservedReads are held back for work that must succeed after the scan: the
// cross-check view, and a margin for the write path's own accounting.
const reservedReads = 2

// receiverConfig is the deployed CREQueueExecutor's own state. It is read every
// tick rather than mirrored from config: `lastSequence` in particular is not
// state the workflow owns (see docs/envelope.md).
type receiverConfig struct {
	ChainSelector        uint64
	MaxReportAge         uint64
	LastSequence         uint64
	NextBatchIDToProcess uint64
	MinBatchAge          uint64
	MaxUsersPerUpkeep    uint64
	Paused               bool
}

// preamble is everything the decision needs before the batch scan, gathered in
// a single multicall.
type preamble struct {
	// protocol is the resolved address book. Every protocol address comes from
	// the Registry, never from config, so a redeploy that re-registers a
	// contract cannot leave the keeper pointed at a dead address.
	protocol       registry.Protocol
	exitQueue      registry.Contract
	controller     registry.Contract
	receiver       receiverConfig
	protocolPaused bool
	currentBatchID uint64
	maxProcessing  uint64
	// blockTimestamp is the observed block's clock — the one the contracts
	// recorded their timestamps against, and the one `observedAt` must come
	// from. See evmread.BlockTimestamp.
	blockTimestamp uint64
}

// readPreamble resolves addresses, receiver config, pause flags and queue-wide
// facts.
//
// It costs two chain reads: the Registry lookups have to resolve before the
// ExitQueue and pause reads can name their targets, so they cannot share a
// batch.
func readPreamble(c *evmread.Caller, reg, receiver common.Address, b *evmread.Budget) (preamble, error) {
	if !b.Take(3) {
		return preamble{}, fmt.Errorf("read budget exhausted before the preamble")
	}

	// Dispatched first so it is in flight while the multicalls resolve.
	tsPromise := c.BlockTimestamp()

	// Round 1: the address book, plus everything reachable from the receiver
	// address alone. The Registry lookups all land in one chain read.
	receiverFields := []struct {
		name   string
		abi    everabi.Name
		method string
	}{
		{"CHAIN_SELECTOR", everabi.ICREReceiverBase, "CHAIN_SELECTOR"},
		{"MAX_REPORT_AGE", everabi.ICREReceiverBase, "MAX_REPORT_AGE"},
		{"lastSequence", everabi.ICREReceiverBase, "lastSequence"},
		{"nextBatchIdToProcess", everabi.ICREQueueExecutor, "nextBatchIdToProcess"},
		{"minBatchAge", everabi.ICREQueueExecutor, "minBatchAge"},
		{"maxUsersPerUpkeep", everabi.ICREQueueExecutor, "maxUsersPerUpkeep"},
	}
	// The receiver reads do not depend on the resolved addresses, so they ride
	// in the same chain read as the address book rather than paying for a round
	// of their own — the budget is 15 for the whole tick.
	round1 := make([]evmread.SubCall, 0, len(receiverFields)+1)
	for _, f := range receiverFields {
		round1 = append(round1, evmread.SubCall{To: receiver, ABI: f.abi, Method: f.method})
	}
	round1 = append(round1, evmread.SubCall{To: receiver, ABI: everabi.Pausable, Method: "paused"})

	protocol, results, err := registry.ResolveWith(c, reg,
		[]registry.Key{registry.Controller, registry.ExitQueue, registry.AMM}, round1)
	if err != nil {
		return preamble{}, err
	}

	out := preamble{protocol: protocol}
	out.controller, err = protocol.Controller()
	if err != nil {
		return preamble{}, err
	}
	out.exitQueue, err = protocol.ExitQueue()
	if err != nil {
		return preamble{}, err
	}
	amm, err := protocol.AMM()
	if err != nil {
		return preamble{}, err
	}

	// CHAIN_SELECTOR / MAX_REPORT_AGE / lastSequence are uint64 on-chain; the
	// executor knobs are uint256. SubResult.Uint64 accepts either shape.
	nums := make([]uint64, len(receiverFields))
	for i, f := range receiverFields {
		if nums[i], err = results[i].Uint64(f.name); err != nil {
			return preamble{}, err
		}
	}
	out.receiver = receiverConfig{
		ChainSelector:        nums[0],
		MaxReportAge:         nums[1],
		LastSequence:         nums[2],
		NextBatchIDToProcess: nums[3],
		MinBatchAge:          nums[4],
		MaxUsersPerUpkeep:    nums[5],
	}
	if out.receiver.Paused, err = results[len(results)-1].Bool("receiver.paused"); err != nil {
		return preamble{}, err
	}

	// Round 2: everything that needed the resolved addresses. Each sub-call is
	// built from its Contract, so the address and the ABI cannot be mismatched.
	round2 := []evmread.SubCall{
		out.exitQueue.Sub("currentBatchId"),
		out.exitQueue.Sub("MAX_BATCH_PROCESSING_TIME"),
		out.controller.Paused(),
		out.exitQueue.Paused(),
		amm.Paused(),
	}
	results, err = c.Aggregate(round2, false).Await()
	if err != nil {
		return preamble{}, fmt.Errorf("reading queue state and pause flags: %w", err)
	}

	if out.currentBatchID, err = results[0].Uint64("currentBatchId"); err != nil {
		return preamble{}, err
	}
	if out.maxProcessing, err = results[1].Uint64("MAX_BATCH_PROCESSING_TIME"); err != nil {
		return preamble{}, err
	}
	out.protocolPaused = out.receiver.Paused
	for i, label := range []string{"controller.paused", "exitQueue.paused", "amm.paused"} {
		paused, err := results[2+i].Bool(label)
		if err != nil {
			return preamble{}, err
		}
		out.protocolPaused = out.protocolPaused || paused
	}

	if out.blockTimestamp, err = tsPromise.Await(); err != nil {
		return preamble{}, fmt.Errorf("reading block timestamp: %w", err)
	}

	return out, nil
}

// readUpkeepStatus reads the gas-bounded on-chain cross-check view.
//
// A failure here is not fatal: the view is a cross-check, not the decision. The
// caller logs the divergence as unavailable and proceeds, because losing the
// cross-check should not stop the keeper from working.
func readUpkeepStatus(c *evmread.Caller, receiver common.Address, b *evmread.Budget) (queue.UpkeepStatus, error) {
	if !b.Take(1) {
		return queue.UpkeepStatus{}, fmt.Errorf("read budget exhausted before the cross-check")
	}
	vals, err := c.Call(receiver, everabi.ICREQueueExecutor, "queueUpkeepStatus").Await()
	if err != nil {
		return queue.UpkeepStatus{}, err
	}
	return queue.DecodeUpkeepStatus(vals)
}

// readQueueState performs the batch scan within whatever budget remains.
//
// Depth is not a fixed number: it is whatever the remaining reads buy. On a
// healthy queue that is far past the on-chain view's 25-batch window, which is
// where W1's advantage comes from; after a long stall it degrades to a
// truncated scan rather than an aborted execution.
func readQueueState(
	c *evmread.Caller,
	p preamble,
	maxScan uint64,
	b *evmread.Budget,
) (queue.State, error) {
	rc := p.receiver
	state := queue.State{
		Now:                    p.blockTimestamp,
		Paused:                 p.protocolPaused,
		CurrentBatchID:         p.currentBatchID,
		NextBatchIDToProcess:   rc.NextBatchIDToProcess,
		ControllerBalance:      new(big.Int),
		MaxBatchProcessingTime: p.maxProcessing,
		MinBatchAge:            rc.MinBatchAge,
		MaxUsersPerUpkeep:      rc.MaxUsersPerUpkeep,
		Batches:                map[uint64]queue.Batch{},
	}

	// A paused protocol means every action reverts, so skip the scan entirely
	// rather than spend the budget proving it.
	if state.Paused {
		return state, nil
	}

	if !b.Take(1) {
		return queue.State{}, fmt.Errorf("read budget exhausted before the controller balance")
	}
	balance, err := c.BalanceAt(p.controller.Address).Await()
	if err != nil {
		return queue.State{}, fmt.Errorf("reading controller balance: %w", err)
	}
	state.ControllerBalance = balance

	// Scan range: the receiver's cursor through the current batch. The current
	// batch is included because PriceBatch needs its age and unprocessed count.
	first := rc.NextBatchIDToProcess
	last := p.currentBatchID
	if first > last {
		first = last
	}
	if maxScan > 0 && last-first >= maxScan {
		last = first + maxScan - 1
	}

	// Phase 1: batchInfo + unprocessedUsersCount for every batch in range.
	// batchInfo dominates the response size at 160 bytes.
	perResult := evmread.EstimateResultBytes(160)
	var infoCalls []evmread.SubCall
	var ids []uint64
	for id := first; id <= last; id++ {
		ids = append(ids, id)
		arg := new(big.Int).SetUint64(id)
		infoCalls = append(infoCalls,
			p.exitQueue.Sub("batchInfo", arg),
			p.exitQueue.Sub("unprocessedUsersCount", arg),
		)
	}

	chunks := evmread.ChunkSubCalls(infoCalls, perResult)
	// Leave at least two reads for the user and request phases, or the scan
	// finds batches it cannot evaluate.
	affordable := b.Remaining() - 2
	if affordable < 0 {
		affordable = 0
	}
	if len(chunks) > affordable {
		chunks = chunks[:affordable]
	}

	scanned := 0
	for _, chunk := range chunks {
		if !b.Take(1) {
			break
		}
		results, err := c.Aggregate(chunk, false).Await()
		if err != nil {
			return queue.State{}, fmt.Errorf("reading batch info: %w", err)
		}
		for i := 0; i < len(results); i += 2 {
			id := ids[(scanned+i)/2]
			batch, err := queue.DecodeBatchInfo(id, results[i].Values)
			if err != nil {
				return queue.State{}, err
			}
			if batch.UnprocessedCount, err = results[i+1].Uint64("unprocessedUsersCount"); err != nil {
				return queue.State{}, err
			}
			state.Batches[id] = batch
		}
		scanned += len(results)
	}

	// Record where the scan actually stopped, so a "found nothing" result is
	// distinguishable from "did not look".
	reached := first + uint64(scanned/2)
	if reached <= last && reached < p.currentBatchID {
		state.ScanTruncatedAt = reached
	}

	// Phase 2: user lists for batches the receiver could act on. An unpriced
	// batch is never processable, so its users are irrelevant.
	type pending struct {
		id    uint64
		limit uint64
	}
	var candidates []pending
	for _, id := range ids {
		b, ok := state.Batches[id]
		if !ok || !b.CanBeProcessed || b.UnprocessedCount == 0 {
			continue
		}
		limit := b.UnprocessedCount
		if limit > rc.MaxUsersPerUpkeep {
			limit = rc.MaxUsersPerUpkeep
		}
		candidates = append(candidates, pending{id: id, limit: limit})
	}
	if len(candidates) == 0 {
		return state, nil
	}

	// Only the oldest candidate can be chosen — Decide takes the first batch
	// with affordable work — so reading users for the rest would spend budget
	// on batches that cannot be picked this tick.
	candidates = candidates[:1]

	userCalls := make([]evmread.SubCall, 0, len(candidates))
	for _, cand := range candidates {
		userCalls = append(userCalls, p.exitQueue.Sub("unprocessedUsers",
			new(big.Int).SetUint64(cand.id), new(big.Int), new(big.Int).SetUint64(cand.limit)))
	}
	if !b.Take(1) {
		state.ScanTruncatedAt = first
		return state, nil
	}
	userResults, err := c.Aggregate(userCalls, false).Await()
	if err != nil {
		return queue.State{}, fmt.Errorf("reading unprocessed users: %w", err)
	}

	// Phase 3: per-request detail for those users.
	var requestCalls []evmread.SubCall
	type owner struct {
		batchID uint64
		user    common.Address
	}
	var owners []owner
	for i, cand := range candidates {
		if len(userResults[i].Values) != 1 {
			return queue.State{}, fmt.Errorf("unprocessedUsers returned %d values, want 1", len(userResults[i].Values))
		}
		users, err := evmread.Addresses(userResults[i].Values[0], "unprocessedUsers")
		if err != nil {
			return queue.State{}, err
		}
		for _, u := range users {
			owners = append(owners, owner{batchID: cand.id, user: u})
			requestCalls = append(requestCalls,
				p.exitQueue.Sub("requestInfo", new(big.Int).SetUint64(cand.id), u))
		}
	}
	if len(requestCalls) == 0 {
		return state, nil
	}

	done := 0
	for _, chunk := range evmread.ChunkSubCalls(requestCalls, perResult) {
		if !b.Take(1) {
			break
		}
		results, err := c.Aggregate(chunk, false).Await()
		if err != nil {
			return queue.State{}, fmt.Errorf("reading request info: %w", err)
		}
		for i, r := range results {
			o := owners[done+i]
			req, err := queue.DecodeRequestInfo(o.user, r.Values)
			if err != nil {
				return queue.State{}, err
			}
			batch := state.Batches[o.batchID]
			batch.Requests = append(batch.Requests, req)
			state.Batches[o.batchID] = batch
		}
		done += len(results)
	}
	if done < len(requestCalls) && state.ScanTruncatedAt == 0 {
		state.ScanTruncatedAt = candidates[0].id
	}

	return state, nil
}
