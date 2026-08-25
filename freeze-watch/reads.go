//go:build wasip1

package main

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	everabi "github.com/everstrat-xyz/keepers/contracts/evm/src/abi"
	"github.com/everstrat-xyz/keepers/pkg/chains"
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

	// Round 1: the address book. W4 watches more contracts than the keepers do
	// — it is the thing that notices when one of them is wrong.
	protocol, err := registry.Resolve(c, reg,
		registry.Controller, registry.ExitQueue, registry.AMM,
		registry.StrategyManager, registry.Oracle, registry.QueueKeeperExecutor)
	if err != nil {
		return obs, err
	}

	controller := protocol.MustGet(registry.Controller)
	exitQueue := protocol.MustGet(registry.ExitQueue)
	amm := protocol.MustGet(registry.AMM)
	strategyManager := protocol.MustGet(registry.StrategyManager)
	oracle := protocol.MustGet(registry.Oracle)
	queueExecutor := protocol.MustGet(registry.QueueKeeperExecutor)
	obs.Protocol = protocol

	// Round 2: pause flags, queue geometry, strategy list.
	round2 := []evmread.SubCall{
		controller.Paused(),
		exitQueue.Paused(),
		amm.Paused(),
		strategyManager.Paused(),
		exitQueue.Sub("currentBatchId"),
		exitQueue.Sub("MAX_BATCH_PROCESSING_TIME"),
		queueExecutor.Sub("nextLiveBatchIdToProcess"),
		strategyManager.Sub("strategies"),
	}
	results, err := c.Aggregate(round2, false).Await()
	if err != nil {
		return obs, fmt.Errorf("reading protocol status: %w", err)
	}

	flags := make([]bool, 4)
	for i := range flags {
		if flags[i], err = results[i].Bool("paused"); err != nil {
			return obs, err
		}
	}
	obs.ControllerPaused, obs.ExitQueuePaused, obs.AMMPaused, obs.StrategyManagerPaused =
		flags[0], flags[1], flags[2], flags[3]

	currentBatchID, err := results[4].Uint64("currentBatchId")
	if err != nil {
		return obs, err
	}
	if obs.MaxBatchProcessingTime, err = results[5].Uint64("MAX_BATCH_PROCESSING_TIME"); err != nil {
		return obs, err
	}
	cursor, err := results[6].Uint64("nextLiveBatchIdToProcess")
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
	if err := readOracle(c, oracle, &obs, b); err != nil {
		return obs, err
	}
	readKeepers(c, config, &obs, b)

	return obs, nil
}

// readBatches checks the oldest unprocessed batches for escape-hatch exposure.
func readBatches(
	c *evmread.Caller,
	exitQueue registry.Contract,
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
			exitQueue.Sub("batchInfo", arg),
			exitQueue.Sub("unprocessedUsersCount", arg),
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
			count, err := results[i+1].Uint64("unprocessedUsersCount")
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
			paused, err := results[i].Bool("strategy.paused")
			if err != nil {
				return err
			}
			healthy, err := results[i+1].Bool("strategy.isHealthy")
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
	oracle registry.Contract,
	obs *freezewatch.Observation,
	b *evmread.Budget,
) error {
	if !b.Take(1) {
		return nil
	}

	vals, err := c.Call(oracle.Address, everabi.IOracle, "getSupportedTokens").Await()
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
		calls = append(calls, oracle.Sub("getUsdPrice", tok))
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
		name        string
		configField string
		addr        string
		abi         everabi.Name
		view        string
	}
	targets := []target{
		{"queue-keeper", "queueExecutorAddress", config.QueueExecutorAddress,
			everabi.IQueueKeeperExecutor, "queueUpkeepStatus"},
		{"strategy-keeper", "strategyExecutorAddress", config.StrategyExecutorAddress,
			everabi.IStrategyKeeperExecutor, "strategyUpkeepStatus"},
	}

	for _, t := range targets {
		// Same validation W1 and W2 apply to their own executor address, so an
		// address W4 reports as healthy is one they could actually call.
		addr, err := chains.ParseAddress(t.configField, t.addr)
		if err != nil {
			continue // not deployed yet, or a placeholder; nothing to watch
		}
		if !b.Take(1) {
			return
		}

		// The CRE-era workflow-binding views are gone. The Gelato-era
		// equivalent of "a task is wired up" is the executor having at least
		// one allowlisted automation caller — an empty allowlist means every
		// perform() reverts KeeperExecutorNoAllowedCallers, which is exactly
		// the "bound but broken" state W4 exists to catch. When the dedicated
		// proxy address is configured, isExecutorCaller must also return true
		// for it.
		calls := []evmread.SubCall{
			{To: addr, ABI: everabi.IKeeperExecutorBase, Method: "executorCallerCount"},
			{To: addr, ABI: everabi.Pausable, Method: "paused"},
			{To: addr, ABI: t.abi, Method: t.view},
		}
		if proxy, err := chains.ParseAddress("gelatoProxyAddress", config.GelatoProxyAddress); err == nil {
			calls = append(calls, evmread.SubCall{
				To: addr, ABI: everabi.IKeeperExecutorBase, Method: "isExecutorCaller",
				Args: []interface{}{proxy},
			})
		}
		results, err := c.Aggregate(calls, true).Await()
		if err != nil {
			continue
		}

		k := freezewatch.KeeperHealth{Name: t.name}
		if results[0].Success && len(results[0].Values) == 1 {
			if count, err := evmread.Uint64(results[0].Values[0], "executorCallerCount"); err == nil {
				k.Bound = count > 0
			}
		}
		if len(calls) == 4 && results[3].Success && len(results[3].Values) == 1 {
			if allowed, err := results[3].Bool("isExecutorCaller"); err == nil {
				// A configured proxy missing from the allowlist means the task
				// will fire and revert — bound, but broken.
				k.Bound = k.Bound && allowed
			}
		}
		if results[1].Success {
			paused, err := results[1].Bool("executor.paused")
			if err == nil {
				k.Paused = paused
			}
		}
		if results[2].Success && len(results[2].Values) > 0 {
			if action, ok := results[2].Values[0].(uint8); ok {
				k.UpkeepAvailable = action != 0
			}
		}
		obs.Keepers = append(obs.Keepers, k)
	}
}
