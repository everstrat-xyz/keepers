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
//	1  multicall   per-strategy health/capacity/cooldown/fees
//	1  multicall   queue batchInfo + counts for the bounded redemption scan
//	M  multicall   unprocessedUsers + requestInfo for those batches
//	1  call        strategyUpkeepStatus cross-check
//
// W2's redemption scan is capped at strategy.MaxBatchScan / MaxUsersCostScan on
// purpose — matching the contract, not exceeding it. See strategy.Decide.

const reservedReads = 2

type protocolAddresses struct {
	Controller      common.Address
	ExitQueue       common.Address
	AMM             common.Address
	StrategyManager common.Address
	QueueExecutor   common.Address
}

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
	addrs          protocolAddresses
	receiver       receiverConfig
	protocolPaused bool
	blockTimestamp uint64

	currentBatchID uint64
	queueCursor    uint64
	maxProcessing  uint64
	eveBasePrice   *big.Int
	ammFreeBalance *big.Int
	performanceBps *big.Int
	strategies     []common.Address
}

func readPreamble(c *evmread.Caller, reg, receiver common.Address, b *evmread.Budget) (preamble, error) {
	if !b.Take(3) {
		return preamble{}, fmt.Errorf("read budget exhausted before the preamble")
	}

	tsPromise := c.BlockTimestamp()

	// Round 1: registry lookups plus everything reachable from the receiver.
	keys := []common.Hash{
		registry.KeyController, registry.KeyExitQueue, registry.KeyAMM,
		registry.KeyStrategyManager, registry.KeyQueueKeeperExecutor,
	}
	round1 := make([]evmread.SubCall, 0, len(keys)+12)
	for _, k := range keys {
		round1 = append(round1, evmread.SubCall{
			To: reg, ABI: everabi.IRegistry, Method: "getContractByKey", Args: []any{k},
		})
	}

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
	for _, f := range uintFields {
		round1 = append(round1, evmread.SubCall{To: receiver, ABI: f.abi, Method: f.method})
	}

	bigFields := []string{
		"controllerReserveETH", "minDepositETH", "minWithdrawETH",
		"minHarvestETH", "exitLiquidityTargetETH", "minExitLiquidityTopUpETH",
	}
	for _, m := range bigFields {
		round1 = append(round1, evmread.SubCall{To: receiver, ABI: everabi.ICREStrategyExecutor, Method: m})
	}
	round1 = append(round1, evmread.SubCall{To: receiver, ABI: everabi.Pausable, Method: "paused"})

	results, err := c.Aggregate(round1, false).Await()
	if err != nil {
		return preamble{}, fmt.Errorf("reading protocol addresses and receiver config: %w", err)
	}

	var out preamble
	targets := []*common.Address{
		&out.addrs.Controller, &out.addrs.ExitQueue, &out.addrs.AMM,
		&out.addrs.StrategyManager, &out.addrs.QueueExecutor,
	}
	for i, k := range keys {
		addr, err := singleAddress(results[i], registry.Name(k))
		if err != nil {
			return preamble{}, err
		}
		if addr == (common.Address{}) {
			return preamble{}, fmt.Errorf("registry %s has no address for %s", reg, registry.Name(k))
		}
		*targets[i] = addr
	}

	base := len(keys)
	nums := make([]uint64, len(uintFields))
	for i, f := range uintFields {
		r := results[base+i]
		if len(r.Values) != 1 {
			return preamble{}, fmt.Errorf("%s returned %d values, want 1", f.name, len(r.Values))
		}
		if n, ok := r.Values[0].(uint64); ok {
			nums[i] = n
			continue
		}
		if nums[i], err = evmread.Uint64(r.Values[0], f.name); err != nil {
			return preamble{}, err
		}
	}

	base += len(uintFields)
	bigs := make([]*big.Int, len(bigFields))
	for i, name := range bigFields {
		if bigs[i], err = singleBigInt(results[base+i], name); err != nil {
			return preamble{}, err
		}
	}

	paused, err := singleBool(results[len(results)-1], "receiver.paused")
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

	// Round 2: everything that needed the resolved addresses.
	round2 := []evmread.SubCall{
		{To: out.addrs.ExitQueue, ABI: everabi.IExitQueue, Method: "currentBatchId"},
		{To: out.addrs.ExitQueue, ABI: everabi.IExitQueue, Method: "MAX_BATCH_PROCESSING_TIME"},
		{To: out.addrs.QueueExecutor, ABI: everabi.ICREQueueExecutor, Method: "nextLiveBatchIdToProcess"},
		{To: out.addrs.AMM, ABI: everabi.IAMM, Method: "freeBalance"},
		{To: out.addrs.AMM, ABI: everabi.IAMM, Method: "eveBasePriceInETH"},
		{To: out.addrs.StrategyManager, ABI: everabi.IStrategyManager, Method: "performanceFeeBps"},
		{To: out.addrs.StrategyManager, ABI: everabi.IStrategyManager, Method: "strategies"},
		{To: out.addrs.Controller, ABI: everabi.Pausable, Method: "paused"},
		{To: out.addrs.StrategyManager, ABI: everabi.Pausable, Method: "paused"},
	}
	results, err = c.Aggregate(round2, false).Await()
	if err != nil {
		return preamble{}, fmt.Errorf("reading protocol state: %w", err)
	}

	if out.currentBatchID, err = singleUint64(results[0], "currentBatchId"); err != nil {
		return preamble{}, err
	}
	if out.maxProcessing, err = singleUint64(results[1], "MAX_BATCH_PROCESSING_TIME"); err != nil {
		return preamble{}, err
	}
	if out.queueCursor, err = singleUint64(results[2], "nextLiveBatchIdToProcess"); err != nil {
		return preamble{}, err
	}
	if out.ammFreeBalance, err = singleBigInt(results[3], "freeBalance"); err != nil {
		return preamble{}, err
	}
	// The AMM reverts AMMZeroTotalSupply before anything is minted; treat that
	// as a zero price rather than a failed tick, since with no supply there are
	// no redemptions to price either.
	if out.eveBasePrice, err = singleBigInt(results[4], "eveBasePriceInETH"); err != nil {
		return preamble{}, err
	}
	if out.performanceBps, err = singleBigInt(results[5], "performanceFeeBps"); err != nil {
		return preamble{}, err
	}
	if len(results[6].Values) != 1 {
		return preamble{}, fmt.Errorf("strategies() returned %d values, want 1", len(results[6].Values))
	}
	if out.strategies, err = evmread.Addresses(results[6].Values[0], "strategies"); err != nil {
		return preamble{}, err
	}

	out.protocolPaused = out.receiver.Paused
	for i, label := range []string{"controller.paused", "strategyManager.paused"} {
		p, err := singleBool(results[7+i], label)
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
	balance, err := c.BalanceAt(p.addrs.Controller).Await()
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

	// Six sub-calls per strategy: paused, isHealthy, maxDeposit, maxWithdrawal,
	// isStrategyInDepositCooldown, pendingPerformanceFeeInETH.
	var calls []evmread.SubCall
	for _, addr := range p.strategies {
		calls = append(calls,
			evmread.SubCall{To: addr, ABI: everabi.IStrategy, Method: "paused"},
			evmread.SubCall{To: addr, ABI: everabi.IStrategy, Method: "isHealthy"},
			evmread.SubCall{To: addr, ABI: everabi.IStrategy, Method: "maxDeposit"},
			evmread.SubCall{To: addr, ABI: everabi.IStrategy, Method: "maxWithdrawal"},
			evmread.SubCall{To: p.addrs.StrategyManager, ABI: everabi.IStrategyManager,
				Method: "isStrategyInDepositCooldown", Args: []any{addr}},
			evmread.SubCall{To: p.addrs.StrategyManager, ABI: everabi.IStrategyManager,
				Method: "pendingPerformanceFeeInETH", Args: []any{addr}},
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
		base := i * 6
		s := strategy.Strategy{Address: addr}
		var err error
		if s.Paused, err = singleBool(values[base], "strategy.paused"); err != nil {
			return nil, err
		}
		if s.Healthy, err = singleBool(values[base+1], "strategy.isHealthy"); err != nil {
			return nil, err
		}
		if s.MaxDeposit, err = singleBigInt(values[base+2], "strategy.maxDeposit"); err != nil {
			return nil, err
		}
		if s.MaxWithdrawal, err = singleBigInt(values[base+3], "strategy.maxWithdrawal"); err != nil {
			return nil, err
		}
		if s.InDepositCooldown, err = singleBool(values[base+4], "isStrategyInDepositCooldown"); err != nil {
			return nil, err
		}
		if s.PendingPerformanceFeeETH, err = singleBigInt(values[base+5], "pendingPerformanceFeeInETH"); err != nil {
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

	// Phase 1: batchInfo + count for each batch in the bounded window, plus the
	// current batch (needed for its totalTokensToBurn).
	var ids []uint64
	var calls []evmread.SubCall
	for id := first; id <= last; id++ {
		ids = append(ids, id)
		arg := new(big.Int).SetUint64(id)
		calls = append(calls,
			evmread.SubCall{To: p.addrs.ExitQueue, ABI: everabi.IExitQueue, Method: "batchInfo", Args: []any{arg}},
			evmread.SubCall{To: p.addrs.ExitQueue, ABI: everabi.IExitQueue, Method: "unprocessedUsersCount", Args: []any{arg}},
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
			if batch.UnprocessedCount, err = singleUint64(results[i+1], "unprocessedUsersCount"); err != nil {
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
	// is priced from totalTokensToBurn instead, so it is skipped here.
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
		userCalls = append(userCalls, evmread.SubCall{
			To: p.addrs.ExitQueue, ABI: everabi.IExitQueue, Method: "unprocessedUsers",
			Args: []any{new(big.Int).SetUint64(id), new(big.Int), new(big.Int).SetUint64(limit)},
		})
	}

	if len(userCalls) > 0 {
		if !b.Take(1) {
			truncated = true
		} else {
			userResults, err := c.Aggregate(userCalls, false).Await()
			if err != nil {
				return nil, false, fmt.Errorf("reading unprocessed users: %w", err)
			}

			var reqCalls []evmread.SubCall
			var reqOwners []uint64
			for i, id := range userIDs {
				if len(userResults[i].Values) != 1 {
					return nil, false, fmt.Errorf("unprocessedUsers returned %d values, want 1", len(userResults[i].Values))
				}
				users, err := evmread.Addresses(userResults[i].Values[0], "unprocessedUsers")
				if err != nil {
					return nil, false, err
				}
				for _, u := range users {
					reqOwners = append(reqOwners, id)
					reqCalls = append(reqCalls, evmread.SubCall{
						To: p.addrs.ExitQueue, ABI: everabi.IExitQueue, Method: "requestInfo",
						Args: []any{new(big.Int).SetUint64(id), u},
					})
				}
			}

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
	}

	needs, err := strategy.PendingRedemptionNeedsETH(
		batches, p.queueCursor, p.currentBatchID, p.maxProcessing, p.blockTimestamp, p.eveBasePrice)
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
	totalTokensToBurn, err := evmread.BigInt(vals[2], "batchInfo.totalTokensToBurn")
	if err != nil {
		return strategy.QueueBatch{}, err
	}
	pricedAt, err := evmread.Uint64(vals[4], "batchInfo.pricedAt")
	if err != nil {
		return strategy.QueueBatch{}, err
	}
	return strategy.QueueBatch{
		ID:                id,
		CanBeProcessed:    canBeProcessed,
		FinalEvePrice:     finalEvePrice,
		TotalTokensToBurn: totalTokensToBurn,
		PricedAt:          pricedAt,
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

// ---------- single-value helpers ----------

func singleAddress(r evmread.SubResult, field string) (common.Address, error) {
	if len(r.Values) != 1 {
		return common.Address{}, fmt.Errorf("%s returned %d values, want 1", field, len(r.Values))
	}
	return evmread.Address(r.Values[0], field)
}

func singleUint64(r evmread.SubResult, field string) (uint64, error) {
	if len(r.Values) != 1 {
		return 0, fmt.Errorf("%s returned %d values, want 1", field, len(r.Values))
	}
	return evmread.Uint64(r.Values[0], field)
}

func singleBigInt(r evmread.SubResult, field string) (*big.Int, error) {
	if len(r.Values) != 1 {
		return nil, fmt.Errorf("%s returned %d values, want 1", field, len(r.Values))
	}
	return evmread.BigInt(r.Values[0], field)
}

func singleBool(r evmread.SubResult, field string) (bool, error) {
	if len(r.Values) != 1 {
		return false, fmt.Errorf("%s returned %d values, want 1", field, len(r.Values))
	}
	return evmread.Bool(r.Values[0], field)
}
