//go:build wasip1

package main

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	everabi "github.com/everstrat-xyz/keepers/contracts/evm/src/abi"
	"github.com/everstrat-xyz/keepers/pkg/evmread"
	"github.com/everstrat-xyz/keepers/pkg/freezewatch"
	"github.com/everstrat-xyz/keepers/pkg/registry"
)

// batchWatchWindow is how many batches behind the cursor W4 checks for
// escape-hatch exposure.
//
// It does not need W1's depth: the batches at risk are the oldest unprocessed
// ones, which sit at the cursor. Watching a short window keeps W4 well inside
// the read budget so it stays available when the protocol is unhealthy —
// exactly when a read-heavy monitor would be most likely to fail.
const batchWatchWindow = 12

// readObservation gathers everything Evaluate needs.
//
// W4 tolerates partial reads: a contract it cannot reach produces no alert for
// that check rather than a failed tick. A monitor that goes silent when the
// protocol misbehaves is worse than one with a blind spot it reports.
func readObservation(
	c *evmread.Caller,
	config *Config,
	reg common.Address,
	b *evmread.Budget,
) (freezewatch.Observation, error) {
	obs := freezewatch.Observation{}

	if !b.Take(2) {
		return obs, fmt.Errorf("read budget exhausted before the preamble")
	}
	tsPromise := c.BlockTimestamp()

	// Round 1: address resolution.
	keys := []common.Hash{
		registry.KeyController, registry.KeyExitQueue, registry.KeyAMM,
		registry.KeyStrategyManager, registry.KeyOracle, registry.KeyQueueKeeperExecutor,
	}
	round1 := make([]evmread.SubCall, len(keys))
	for i, k := range keys {
		round1[i] = evmread.SubCall{To: reg, ABI: everabi.IRegistry, Method: "getContractByKey", Args: []any{k}}
	}
	results, err := c.Aggregate(round1, false).Await()
	if err != nil {
		return obs, fmt.Errorf("resolving protocol addresses: %w", err)
	}

	addrs := make([]common.Address, len(keys))
	for i, k := range keys {
		if len(results[i].Values) != 1 {
			return obs, fmt.Errorf("%s lookup returned %d values", registry.Name(k), len(results[i].Values))
		}
		if addrs[i], err = evmread.Address(results[i].Values[0], registry.Name(k)); err != nil {
			return obs, err
		}
	}
	controller, exitQueue, amm, strategyManager, oracle, queueExecutor :=
		addrs[0], addrs[1], addrs[2], addrs[3], addrs[4], addrs[5]

	// Round 2: pause flags, queue geometry, strategy list.
	round2 := []evmread.SubCall{
		{To: controller, ABI: everabi.Pausable, Method: "paused"},
		{To: exitQueue, ABI: everabi.Pausable, Method: "paused"},
		{To: amm, ABI: everabi.Pausable, Method: "paused"},
		{To: strategyManager, ABI: everabi.Pausable, Method: "paused"},
		{To: exitQueue, ABI: everabi.IExitQueue, Method: "currentBatchId"},
		{To: exitQueue, ABI: everabi.IExitQueue, Method: "MAX_BATCH_PROCESSING_TIME"},
		{To: queueExecutor, ABI: everabi.ICREQueueExecutor, Method: "nextLiveBatchIdToProcess"},
		{To: strategyManager, ABI: everabi.IStrategyManager, Method: "strategies"},
	}
	results, err = c.Aggregate(round2, false).Await()
	if err != nil {
		return obs, fmt.Errorf("reading protocol status: %w", err)
	}

	flags := make([]bool, 4)
	for i := range flags {
		if flags[i], err = boolResult(results[i], "paused"); err != nil {
			return obs, err
		}
	}
	obs.ControllerPaused, obs.ExitQueuePaused, obs.AMMPaused, obs.StrategyManagerPaused =
		flags[0], flags[1], flags[2], flags[3]

	currentBatchID, err := uint64Result(results[4], "currentBatchId")
	if err != nil {
		return obs, err
	}
	if obs.MaxBatchProcessingTime, err = uint64Result(results[5], "MAX_BATCH_PROCESSING_TIME"); err != nil {
		return obs, err
	}
	cursor, err := uint64Result(results[6], "nextLiveBatchIdToProcess")
	if err != nil {
		return obs, err
	}
	if currentBatchID > cursor {
		obs.BacklogBatches = currentBatchID - cursor
	}

	var strategies []common.Address
	if len(results[7].Values) == 1 {
		if strategies, err = evmread.Addresses(results[7].Values[0], "strategies"); err != nil {
			return obs, err
		}
	}

	if obs.Now, err = tsPromise.Await(); err != nil {
		return obs, fmt.Errorf("reading block timestamp: %w", err)
	}

	if err := readBatches(c, exitQueue, cursor, currentBatchID, &obs, b); err != nil {
		return obs, err
	}
	if err := readStrategies(c, strategies, &obs, b); err != nil {
		return obs, err
	}
	if err := readOracle(c, oracle, amm, &obs, b); err != nil {
		return obs, err
	}
	readKeepers(c, config, &obs, b)

	return obs, nil
}

// readBatches checks the oldest unprocessed batches for escape-hatch exposure.
func readBatches(
	c *evmread.Caller,
	exitQueue common.Address,
	cursor, currentBatchID uint64,
	obs *freezewatch.Observation,
	b *evmread.Budget,
) error {
	if cursor >= currentBatchID {
		return nil
	}
	last := currentBatchID - 1
	if last-cursor >= batchWatchWindow {
		last = cursor + batchWatchWindow - 1
	}

	var ids []uint64
	var calls []evmread.SubCall
	for id := cursor; id <= last; id++ {
		ids = append(ids, id)
		arg := new(big.Int).SetUint64(id)
		calls = append(calls,
			evmread.SubCall{To: exitQueue, ABI: everabi.IExitQueue, Method: "batchInfo", Args: []any{arg}},
			evmread.SubCall{To: exitQueue, ABI: everabi.IExitQueue, Method: "unprocessedUsersCount", Args: []any{arg}},
		)
	}

	scanned := 0
	for _, chunk := range evmread.ChunkSubCalls(calls, evmread.EstimateResultBytes(160)) {
		if !b.Take(1) {
			return nil // partial view; better than none
		}
		results, err := c.Aggregate(chunk, false).Await()
		if err != nil {
			return fmt.Errorf("reading batch info: %w", err)
		}
		for i := 0; i < len(results); i += 2 {
			id := ids[(scanned+i)/2]
			if len(results[i].Values) != 5 {
				return fmt.Errorf("batchInfo(%d) returned %d values, want 5", id, len(results[i].Values))
			}
			pricedAt, err := evmread.Uint64(results[i].Values[4], "batchInfo.pricedAt")
			if err != nil {
				return err
			}
			count, err := uint64Result(results[i+1], "unprocessedUsersCount")
			if err != nil {
				return err
			}
			obs.Batches = append(obs.Batches, freezewatch.QueueBatch{
				ID: id, PricedAt: pricedAt, UnprocessedCount: count,
			})
		}
		scanned += len(results)
	}
	return nil
}

func readStrategies(
	c *evmread.Caller,
	strategies []common.Address,
	obs *freezewatch.Observation,
	b *evmread.Budget,
) error {
	if len(strategies) == 0 {
		return nil
	}

	var calls []evmread.SubCall
	for _, addr := range strategies {
		calls = append(calls,
			evmread.SubCall{To: addr, ABI: everabi.IStrategy, Method: "paused"},
			evmread.SubCall{To: addr, ABI: everabi.IStrategy, Method: "isHealthy"},
		)
	}

	done := 0
	for _, chunk := range evmread.ChunkSubCalls(calls, evmread.EstimateResultBytes(32)) {
		if !b.Take(1) {
			return nil
		}
		results, err := c.Aggregate(chunk, false).Await()
		if err != nil {
			return fmt.Errorf("reading strategy health: %w", err)
		}
		for i := 0; i < len(results); i += 2 {
			addr := strategies[(done+i)/2]
			paused, err := boolResult(results[i], "strategy.paused")
			if err != nil {
				return err
			}
			healthy, err := boolResult(results[i+1], "strategy.isHealthy")
			if err != nil {
				return err
			}
			obs.Strategies = append(obs.Strategies, freezewatch.StrategyHealth{
				Address: addr.Hex(), Paused: paused, Healthy: healthy,
			})
		}
		done += len(results)
	}
	return nil
}

// readOracle checks the freshness of the feed NAV depends on.
//
// The pair is discovered from the Oracle's own supported-token list rather than
// configured, so a feed swap does not silently leave W4 watching nothing.
func readOracle(
	c *evmread.Caller,
	oracle, amm common.Address,
	obs *freezewatch.Observation,
	b *evmread.Budget,
) error {
	if !b.Take(1) {
		return nil
	}

	vals, err := c.Call(oracle, everabi.IOracle, "getSupportedTokens").Await()
	if err != nil {
		// An Oracle that cannot be read is itself worth knowing about, but it
		// is not an alert this function can raise without inventing a feed.
		// Deliberately swallowed: a monitor that fails its tick on one bad read
		// goes silent exactly when things are going wrong.
		return nil //nolint:nilerr // partial reads are tolerated by design
	}
	if len(vals) != 1 {
		return nil
	}
	tokens, err := evmread.Addresses(vals[0], "getSupportedTokens")
	if err != nil || len(tokens) == 0 {
		return nil //nolint:nilerr // partial reads are tolerated by design
	}

	calls := make([]evmread.SubCall, 0, len(tokens))
	for _, tok := range tokens {
		calls = append(calls, evmread.SubCall{
			To: oracle, ABI: everabi.IOracle, Method: "getUsdPrice", Args: []any{tok},
		})
	}

	done := 0
	for _, chunk := range evmread.ChunkSubCalls(calls, evmread.EstimateResultBytes(64)) {
		if !b.Take(1) {
			return nil
		}
		// allowFailure: a feed that reverts on staleness is exactly the
		// condition being watched, and must not abort the whole read.
		results, err := c.Aggregate(chunk, true).Await()
		if err != nil {
			return nil //nolint:nilerr // partial reads are tolerated by design
		}
		for i, r := range results {
			tok := tokens[done+i]
			feed := freezewatch.OracleFeed{Pair: tok.Hex() + "/USD"}
			if r.Success && len(r.Values) == 2 {
				if price, err := evmread.BigInt(r.Values[0], "getUsdPrice.price"); err == nil {
					feed.Price = price
				}
				if ts, err := evmread.Uint64(r.Values[1], "getUsdPrice.timestamp"); err == nil {
					feed.UpdatedAt = ts
				}
			}
			obs.Feeds = append(obs.Feeds, feed)
		}
		done += len(results)
	}
	return nil
}

// readKeepers checks receiver liveness for whichever executors are configured.
func readKeepers(c *evmread.Caller, config *Config, obs *freezewatch.Observation, b *evmread.Budget) {
	type target struct {
		name string
		addr string
		abi  everabi.Name
		view string
	}
	targets := []target{
		{"queue-keeper", config.QueueExecutorAddress, everabi.ICREQueueExecutor, "queueUpkeepStatus"},
		{"strategy-keeper", config.StrategyExecutorAddress, everabi.ICREStrategyExecutor, "strategyUpkeepStatus"},
	}

	for _, t := range targets {
		addr, err := chainsParseAddress(t.addr)
		if err != nil {
			continue // not deployed yet; nothing to watch
		}
		if !b.Take(1) {
			return
		}

		calls := []evmread.SubCall{
			{To: addr, ABI: everabi.ICREReceiverBase, Method: "expectedWorkflowId"},
			{To: addr, ABI: everabi.ICREReceiverBase, Method: "expectedAuthor"},
			{To: addr, ABI: everabi.Pausable, Method: "paused"},
			{To: addr, ABI: t.abi, Method: t.view},
		}
		results, err := c.Aggregate(calls, true).Await()
		if err != nil {
			continue
		}

		k := freezewatch.KeeperHealth{Name: t.name}
		if results[0].Success && len(results[0].Values) == 1 {
			if id, ok := results[0].Values[0].([32]byte); ok && id != ([32]byte{}) {
				k.Bound = true
			}
		}
		if !k.Bound && results[1].Success && len(results[1].Values) == 1 {
			if author, err := evmread.Address(results[1].Values[0], "expectedAuthor"); err == nil {
				k.Bound = author != (common.Address{})
			}
		}
		if results[2].Success {
			paused, err := boolResult(results[2], "receiver.paused")
			if err == nil {
				k.Paused = paused
			}
		}
		if results[3].Success && len(results[3].Values) > 0 {
			if action, ok := results[3].Values[0].(uint8); ok {
				k.UpkeepAvailable = action != 0
			}
		}
		obs.Keepers = append(obs.Keepers, k)
	}
}

// chainsParseAddress rejects the zero-address placeholder the scaffold configs
// ship with, so an unconfigured receiver is skipped rather than watched.
func chainsParseAddress(s string) (common.Address, error) {
	if s == "" || !common.IsHexAddress(s) {
		return common.Address{}, fmt.Errorf("not an address: %q", s)
	}
	addr := common.HexToAddress(s)
	if addr == (common.Address{}) {
		return common.Address{}, fmt.Errorf("zero address")
	}
	return addr, nil
}

func uint64Result(r evmread.SubResult, field string) (uint64, error) {
	if len(r.Values) != 1 {
		return 0, fmt.Errorf("%s returned %d values, want 1", field, len(r.Values))
	}
	return evmread.Uint64(r.Values[0], field)
}

func boolResult(r evmread.SubResult, field string) (bool, error) {
	if len(r.Values) != 1 {
		return false, fmt.Errorf("%s returned %d values, want 1", field, len(r.Values))
	}
	return evmread.Bool(r.Values[0], field)
}
