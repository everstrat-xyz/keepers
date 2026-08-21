//go:build wasip1

package main

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	everabi "github.com/everstrat-xyz/keepers/contracts/evm/src/abi"
	"github.com/everstrat-xyz/keepers/pkg/evmread"
	"github.com/everstrat-xyz/keepers/pkg/registry"
	"github.com/everstrat-xyz/keepers/pkg/strategy"
)

// The read plan, against CRE's 15 reads per execution (docs/READ_BUDGET.md):
//
//	1  header      block timestamp (the clock — see docs/envelope.md)
//	1  multicall   registry lookups + receiver thresholds + receiver paused
//	1  multicall   strategy list, fee bps, AMM float, queue cursor, pause flags
//	1  balance     Controller ETH
//	1  multicall   per-strategy health/capacity/cooldown/weight/fees
//	1  multicall   queue batchInfo + counts for the bounded redemption scan
//	M  multicall   unprocessedUsers (chunked; address[] at MaxUsersCostScan)
//	               + requestInfo for those batches
//	1  call        strategyUpkeepStatus cross-check
//
// W2's redemption scan is capped at strategy.MaxBatchScan / MaxUsersCostScan on
// purpose — matching the contract, not exceeding it. See strategy.Decide.
//
// `eveBasePriceInETH` is no longer in the preamble: the current batch's
// unpriced EVE stopped being a liability in contracts PR #43 (M-11), and the
// bounded scan needs no other price.

const reservedReads = 2

// receiverConfig is the deployed CREStrategyExecutor's own state.
type receiverConfig struct {
	ChainSelector uint64
	MaxReportAge  uint64
	LastSequence  uint64
	Paused        bool

	ControllerReserveETH     *big.Int
	MinDepositETH            *big.Int
	MinWithdrawETH           *big.Int
	MinHarvestETH            *big.Int
	ExitLiquidityTargetETH   *big.Int
	MinExitLiquidityTopUpETH *big.Int
	SyncInterval             uint64
	LastSyncAt               uint64
}

type preamble struct {
	// protocol is the resolved address book — see pkg/registry.
	protocol        registry.Protocol
	controller      registry.Contract
	exitQueue       registry.Contract
	amm             registry.Contract
	strategyManager registry.Contract
	queueExecutor   registry.Contract
	receiver        receiverConfig
	protocolPaused  bool
	blockTimestamp  uint64

	currentBatchID uint64
	queueCursor    uint64
	maxProcessing  uint64
	ammFreeBalance *big.Int
	performanceBps *big.Int
	strategies     []common.Address
}

func readPreamble(c *evmread.Caller, reg, receiver common.Address, b *evmread.Budget) (preamble, error) {
	if !b.Take(3) {
		return preamble{}, fmt.Errorf("read budget exhausted before the preamble")
	}

	tsPromise := c.BlockTimestamp()

	// Round 1: the address book plus everything reachable from the receiver
	// address alone. The receiver reads do not depend on the resolved
	// addresses, so they ride in the same chain read (see registry.ResolveWith).
	uintFields := []struct {
		name   string
		abi    everabi.Name
		method string
	}{
		{"CHAIN_SELECTOR", everabi.ICREReceiverBase, "CHAIN_SELECTOR"},
		{"MAX_REPORT_AGE", everabi.ICREReceiverBase, "MAX_REPORT_AGE"},
		{"lastSequence", everabi.ICREReceiverBase, "lastSequence"},
		{"syncInterval", everabi.ICREStrategyExecutor, "syncInterval"},
		{"lastSyncAt", everabi.ICREStrategyExecutor, "lastSyncAt"},
	}
	bigFields := []string{
		"controllerReserveETH", "minDepositETH", "minWithdrawETH",
		"minHarvestETH", "exitLiquidityTargetETH", "minExitLiquidityTopUpETH",
	}

	round1 := make([]evmread.SubCall, 0, len(uintFields)+len(bigFields)+1)
	for _, f := range uintFields {
		round1 = append(round1, evmread.SubCall{To: receiver, ABI: f.abi, Method: f.method})
	}
	for _, m := range bigFields {
		round1 = append(round1, evmread.SubCall{To: receiver, ABI: everabi.ICREStrategyExecutor, Method: m})
	}
	round1 = append(round1, evmread.SubCall{To: receiver, ABI: everabi.Pausable, Method: "paused"})

	protocol, results, err := registry.ResolveWith(c, reg, []registry.Key{
		registry.Controller, registry.ExitQueue, registry.AMM,
		registry.StrategyManager, registry.QueueKeeperExecutor,
	}, round1)
	if err != nil {
		return preamble{}, err
	}

	out := preamble{protocol: protocol}
	for _, bind := range []struct {
		key  registry.Key
		into *registry.Contract
	}{
		{registry.Controller, &out.controller},
		{registry.ExitQueue, &out.exitQueue},
		{registry.AMM, &out.amm},
		{registry.StrategyManager, &out.strategyManager},
		{registry.QueueKeeperExecutor, &out.queueExecutor},
	} {
		if *bind.into, err = protocol.Get(bind.key); err != nil {
			return preamble{}, err
		}
	}

	// The receiver's uint64 immutables and its uint256 knobs both land here;
	// SubResult.Uint64 accepts either shape.
	nums := make([]uint64, len(uintFields))
	for i, f := range uintFields {
		if nums[i], err = results[i].Uint64(f.name); err != nil {
			return preamble{}, err
		}
	}

	bigs := make([]*big.Int, len(bigFields))
	for i, name := range bigFields {
		if bigs[i], err = results[len(uintFields)+i].BigInt(name); err != nil {
			return preamble{}, err
		}
	}

	paused, err := results[len(results)-1].Bool("receiver.paused")
	if err != nil {
		return preamble{}, err
	}

	out.receiver = receiverConfig{
		ChainSelector:            nums[0],
		MaxReportAge:             nums[1],
		LastSequence:             nums[2],
		SyncInterval:             nums[3],
		LastSyncAt:               nums[4],
		ControllerReserveETH:     bigs[0],
		MinDepositETH:            bigs[1],
		MinWithdrawETH:           bigs[2],
		MinHarvestETH:            bigs[3],
		ExitLiquidityTargetETH:   bigs[4],
		MinExitLiquidityTopUpETH: bigs[5],
		Paused:                   paused,
	}

	// Round 2: everything that needed the resolved addresses. Built from the
	// Contracts, so no call site pairs an address with the wrong ABI.
	round2 := []evmread.SubCall{
		out.exitQueue.Sub("currentBatchId"),
		out.exitQueue.Sub("MAX_BATCH_PROCESSING_TIME"),
		out.queueExecutor.Sub("nextLiveBatchIdToProcess"),
		out.amm.Sub("freeBalance"),
		out.strategyManager.Sub("performanceFeeBps"),
		out.strategyManager.Sub("strategies"),
		out.controller.Paused(),
		out.strategyManager.Paused(),
	}
	results, err = c.Aggregate(round2, false).Await()
	if err != nil {
		return preamble{}, fmt.Errorf("reading protocol state: %w", err)
	}

	if out.currentBatchID, err = results[0].Uint64("currentBatchId"); err != nil {
		return preamble{}, err
	}
	if out.maxProcessing, err = results[1].Uint64("MAX_BATCH_PROCESSING_TIME"); err != nil {
		return preamble{}, err
	}
	if out.queueCursor, err = results[2].Uint64("nextLiveBatchIdToProcess"); err != nil {
		return preamble{}, err
	}
	if out.ammFreeBalance, err = results[3].BigInt("freeBalance"); err != nil {
		return preamble{}, err
	}
	if out.performanceBps, err = results[4].BigInt("performanceFeeBps"); err != nil {
		return preamble{}, err
	}
	if len(results[5].Values) != 1 {
		return preamble{}, fmt.Errorf("strategies() returned %d values, want 1", len(results[5].Values))
	}
	if out.strategies, err = evmread.Addresses(results[5].Values[0], "strategies"); err != nil {
		return preamble{}, err
	}

	out.protocolPaused = out.receiver.Paused
	for i, label := range []string{"controller.paused", "strategyManager.paused"} {
		p, err := results[6+i].Bool(label)
		if err != nil {
			return preamble{}, err
		}
		out.protocolPaused = out.protocolPaused || p
	}

	if out.blockTimestamp, err = tsPromise.Await(); err != nil {
		return preamble{}, fmt.Errorf("reading block timestamp: %w", err)
	}
	return out, nil
}

// readUpkeepStatus reads the on-chain cross-check view.
func readUpkeepStatus(c *evmread.Caller, receiver common.Address, b *evmread.Budget) (strategy.UpkeepStatus, error) {
	if !b.Take(1) {
		return strategy.UpkeepStatus{}, fmt.Errorf("read budget exhausted before the cross-check")
	}
	vals, err := c.Call(receiver, everabi.ICREStrategyExecutor, "strategyUpkeepStatus").Await()
	if err != nil {
		return strategy.UpkeepStatus{}, err
	}
	return strategy.DecodeUpkeepStatus(vals)
}

// readStrategyState assembles the decision snapshot.
func readStrategyState(c *evmread.Caller, p preamble, b *evmread.Budget) (strategy.State, error) {
	rc := p.receiver
	state := strategy.State{
		Now:                      p.blockTimestamp,
		Paused:                   p.protocolPaused,
		ControllerBalance:        new(big.Int),
		NeedsETH:                 new(big.Int),
		AMMFreeBalance:           p.ammFreeBalance,
		PerformanceFeeBps:        p.performanceBps,
		ControllerReserveETH:     rc.ControllerReserveETH,
		MinDepositETH:            rc.MinDepositETH,
		MinWithdrawETH:           rc.MinWithdrawETH,
		MinHarvestETH:            rc.MinHarvestETH,
		ExitLiquidityTargetETH:   rc.ExitLiquidityTargetETH,
		MinExitLiquidityTopUpETH: rc.MinExitLiquidityTopUpETH,
		SyncInterval:             rc.SyncInterval,
		LastSyncAt:               rc.LastSyncAt,
	}
	if state.Paused {
		return state, nil
	}

	if !b.Take(1) {
		return strategy.State{}, fmt.Errorf("read budget exhausted before the controller balance")
	}
	balance, err := c.BalanceAt(p.controller.Address).Await()
	if err != nil {
		return strategy.State{}, fmt.Errorf("reading controller balance: %w", err)
	}
	state.ControllerBalance = balance

	if state.Strategies, err = readStrategies(c, p, b); err != nil {
		return strategy.State{}, err
	}

	needs, truncated, err := readPendingNeeds(c, p, b)
	if err != nil {
		return strategy.State{}, err
	}
	state.NeedsETH = needs
	state.ScanTruncated = truncated

	return state, nil
}

// readStrategies gathers per-strategy health, capacity, cooldown and fees.
func readStrategies(c *evmread.Caller, p preamble, b *evmread.Budget) ([]strategy.Strategy, error) {
	if len(p.strategies) == 0 {
		return nil, nil
	}

	// Seven sub-calls per strategy: paused, isHealthy, maxDeposit, maxWithdrawal,
	// isStrategyInDepositCooldown, depositWeight, pendingPerformanceFeeInETH.
	var calls []evmread.SubCall
	for _, addr := range p.strategies {
		calls = append(calls,
			evmread.SubCall{To: addr, ABI: everabi.IStrategy, Method: "paused"},
			evmread.SubCall{To: addr, ABI: everabi.IStrategy, Method: "isHealthy"},
			evmread.SubCall{To: addr, ABI: everabi.IStrategy, Method: "maxDeposit"},
			evmread.SubCall{To: addr, ABI: everabi.IStrategy, Method: "maxWithdrawal"},
			p.strategyManager.Sub("isStrategyInDepositCooldown", addr),
			p.strategyManager.Sub("depositWeight", addr),
			p.strategyManager.Sub("pendingPerformanceFeeInETH", addr),
		)
	}

	perResult := evmread.EstimateResultBytes(32)
	var values []evmread.SubResult
	for _, chunk := range evmread.ChunkSubCalls(calls, perResult) {
		if !b.Take(1) {
			return nil, fmt.Errorf("read budget exhausted while reading strategies (%d registered)", len(p.strategies))
		}
		results, err := c.Aggregate(chunk, false).Await()
		if err != nil {
			return nil, fmt.Errorf("reading strategy state: %w", err)
		}
		values = append(values, results...)
	}

	out := make([]strategy.Strategy, len(p.strategies))
	for i, addr := range p.strategies {
		base := i * 7
		s := strategy.Strategy{Address: addr}
		var err error
		if s.Paused, err = values[base].Bool("strategy.paused"); err != nil {
			return nil, err
		}
		if s.Healthy, err = values[base+1].Bool("strategy.isHealthy"); err != nil {
			return nil, err
		}
		if s.MaxDeposit, err = values[base+2].BigInt("strategy.maxDeposit"); err != nil {
			return nil, err
		}
		if s.MaxWithdrawal, err = values[base+3].BigInt("strategy.maxWithdrawal"); err != nil {
			return nil, err
		}
		if s.InDepositCooldown, err = values[base+4].Bool("isStrategyInDepositCooldown"); err != nil {
			return nil, err
		}
		if s.DepositWeight, err = values[base+5].Uint8("depositWeight"); err != nil {
			return nil, err
		}
		if s.PendingPerformanceFeeETH, err = values[base+6].BigInt("pendingPerformanceFeeInETH"); err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}

// readPendingNeeds reproduces `_pendingRedemptionNeedsETH` from raw queue
// reads, using the contract's own scan caps.
//
// Returns truncated=true when the budget ran out mid-scan, which understates
// the figure and is surfaced in the divergence classification rather than
// silently changing the decision.
func readPendingNeeds(c *evmread.Caller, p preamble, b *evmread.Budget) (*big.Int, bool, error) {
	first := p.queueCursor
	last := p.currentBatchID
	if first > last {
		first = last
	}
	if last-first > strategy.MaxBatchScan {
		last = first + strategy.MaxBatchScan
	}

	// Phase 1: batchInfo + count for each batch in the bounded window. The
	// current batch is included so its (empty) entry decodes like the rest,
	// but it contributes nothing until priced — see strategy.PendingRedemptionNeedsETH.
	var ids []uint64
	var calls []evmread.SubCall
	for id := first; id <= last; id++ {
		ids = append(ids, id)
		arg := new(big.Int).SetUint64(id)
		calls = append(calls,
			p.exitQueue.Sub("batchInfo", arg),
			p.exitQueue.Sub("unprocessedUsersCount", arg),
		)
	}

	batches := map[uint64]strategy.QueueBatch{}
	perResult := evmread.EstimateResultBytes(160)
	scanned := 0
	truncated := false
	for _, chunk := range evmread.ChunkSubCalls(calls, perResult) {
		// Keep one read for the request-detail phase.
		if b.Remaining() <= 1 || !b.Take(1) {
			truncated = true
			break
		}
		results, err := c.Aggregate(chunk, false).Await()
		if err != nil {
			return nil, false, fmt.Errorf("reading queue batches: %w", err)
		}
		for i := 0; i < len(results); i += 2 {
			id := ids[(scanned+i)/2]
			batch, err := decodeQueueBatch(id, results[i].Values)
			if err != nil {
				return nil, false, err
			}
			if batch.UnprocessedCount, err = results[i+1].Uint64("unprocessedUsersCount"); err != nil {
				return nil, false, err
			}
			batches[id] = batch
		}
		scanned += len(results)
	}
	if scanned/2 < len(ids) {
		truncated = true
	}

	// Phase 2: request detail for priced batches with work. The current batch
	// is skipped: it is unpriced, and an unpriced batch is not a liability.
	//
	// unprocessedUsers returns address[] — dynamic, so one Aggregate of every
	// live batch at MaxUsersCostScan blows PayloadSizeLimit (~1.8 kB framed
	// each; three full lists ≈ 5.5 kB) and aborts the tick. Chunk like
	// requestInfo, sized for a full 50-user list.
	var userCalls []evmread.SubCall
	var userIDs []uint64
	for _, id := range ids {
		batch, ok := batches[id]
		if !ok || id == p.currentBatchID || !batch.CanBeProcessed || batch.UnprocessedCount == 0 {
			continue
		}
		limit := batch.UnprocessedCount
		if limit > strategy.MaxUsersCostScan {
			limit = strategy.MaxUsersCostScan
		}
		userIDs = append(userIDs, id)
		userCalls = append(userCalls, p.exitQueue.Sub("unprocessedUsers",
			new(big.Int).SetUint64(id), new(big.Int), new(big.Int).SetUint64(limit)))
	}

	var reqCalls []evmread.SubCall
	var reqOwners []uint64
	if len(userCalls) > 0 {
		// offset word + length word + n addresses.
		userListBytes := evmread.EstimateResultBytes(64 + 32*int(strategy.MaxUsersCostScan))
		listed := 0
		for _, chunk := range evmread.ChunkSubCalls(userCalls, userListBytes) {
			if !b.Take(1) {
				truncated = true
				break
			}
			userResults, err := c.Aggregate(chunk, false).Await()
			if err != nil {
				return nil, false, fmt.Errorf("reading unprocessed users: %w", err)
			}
			for i, r := range userResults {
				if len(r.Values) != 1 {
					return nil, false, fmt.Errorf("unprocessedUsers returned %d values, want 1", len(r.Values))
				}
				users, err := evmread.Addresses(r.Values[0], "unprocessedUsers")
				if err != nil {
					return nil, false, err
				}
				id := userIDs[listed+i]
				for _, u := range users {
					reqOwners = append(reqOwners, id)
					reqCalls = append(reqCalls,
						p.exitQueue.Sub("requestInfo", new(big.Int).SetUint64(id), u))
				}
			}
			listed += len(userResults)
		}
		if listed < len(userCalls) {
			truncated = true
		}
	}

	if len(reqCalls) > 0 {
		done := 0
		for _, chunk := range evmread.ChunkSubCalls(reqCalls, perResult) {
			if !b.Take(1) {
				truncated = true
				break
			}
			results, err := c.Aggregate(chunk, false).Await()
			if err != nil {
				return nil, false, fmt.Errorf("reading request info: %w", err)
			}
			for i, r := range results {
				id := reqOwners[done+i]
				req, err := decodeQueueRequest(r.Values)
				if err != nil {
					return nil, false, err
				}
				batch := batches[id]
				batch.Requests = append(batch.Requests, req)
				batches[id] = batch
			}
			done += len(results)
		}
		if done < len(reqCalls) {
			truncated = true
		}
	}

	needs, err := strategy.PendingRedemptionNeedsETH(
		batches, p.queueCursor, p.currentBatchID, p.maxProcessing, p.blockTimestamp)
	if err != nil {
		return nil, false, err
	}
	return needs, truncated, nil
}

func decodeQueueBatch(id uint64, vals []any) (strategy.QueueBatch, error) {
	if len(vals) != 5 {
		return strategy.QueueBatch{}, fmt.Errorf("batchInfo(%d) returned %d values, want 5", id, len(vals))
	}
	canBeProcessed, err := evmread.Bool(vals[0], "batchInfo.canBeProcessed")
	if err != nil {
		return strategy.QueueBatch{}, err
	}
	finalEvePrice, err := evmread.BigInt(vals[1], "batchInfo.finalEvePrice")
	if err != nil {
		return strategy.QueueBatch{}, err
	}
	pricedAt, err := evmread.Uint64(vals[4], "batchInfo.pricedAt")
	if err != nil {
		return strategy.QueueBatch{}, err
	}
	return strategy.QueueBatch{
		ID:             id,
		CanBeProcessed: canBeProcessed,
		FinalEvePrice:  finalEvePrice,
		PricedAt:       pricedAt,
	}, nil
}

func decodeQueueRequest(vals []any) (strategy.QueueRequest, error) {
	if len(vals) != 5 {
		return strategy.QueueRequest{}, fmt.Errorf("requestInfo returned %d values, want 5", len(vals))
	}
	priceAtRequest, err := evmread.BigInt(vals[2], "requestInfo.evePriceAtRequestTime")
	if err != nil {
		return strategy.QueueRequest{}, err
	}
	tokensToBurn, err := evmread.BigInt(vals[3], "requestInfo.tokensToBurn")
	if err != nil {
		return strategy.QueueRequest{}, err
	}
	tolerance, err := evmread.BigInt(vals[4], "requestInfo.priceTolerance")
	if err != nil {
		return strategy.QueueRequest{}, err
	}
	return strategy.QueueRequest{
		EvePriceAtRequestTime: priceAtRequest,
		TokensToBurn:          tokensToBurn,
		PriceTolerance:        tolerance,
	}, nil
}
