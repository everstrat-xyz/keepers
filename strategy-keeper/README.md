# W2 — strategy-keeper

Implemented, running in **shadow mode**: it decides an action every tick and
cross-checks against the on-chain view, but does not `writeReport` until the
Sepolia cutover ([issue #6](https://github.com/everstrat-xyz/keepers/issues/6)).

On-chain consumer: `CREStrategyExecutor`.

## What a tick does

1. Resolve Controller / ExitQueue / AMM / StrategyManager / queue executor from
   the **Registry**.
2. Read the receiver's thresholds, `lastSyncAt`, `lastSequence` and pause flags.
3. Read per-strategy health, capacity, deposit cooldown and accrued fees.
4. Reproduce `_pendingRedemptionNeedsETH` from raw queue reads.
5. Pick an action in the contract's exact priority order — Rebalance →
   WithdrawShortfall → ProvideExitLiquidity → DepositExcess →
   HarvestPerformanceFees → Sync.
6. Cross-check against `strategyUpkeepStatus()`.
7. Build an **action-only** report and validate it against the receiver's live
   guards. Shadow mode stops here.

## Why W2 does not compute a "more exact" shortfall

[Issue #4](https://github.com/everstrat-xyz/keepers/issues/4) framed W2's
advantage as an exact shortfall versus the truncated on-chain scan. That would
be actively harmful, and the reason matters:

> `_processReport` re-derives every quantity with the **same** bounded helpers
> the view uses — `MAX_BATCH_SCAN` batches, `MAX_USERS_COST_SCAN` users. A
> workflow computing a truer shortfall would propose `WithdrawShortfall` in
> states where the receiver's own recomputation sees none, and revert with
> `KeeperExecutorNoUpkeepNeeded` every time.

W1 is the opposite case: `_processReport` validates a `ProcessRequests` claim
per batch with no window, so scanning deeper genuinely wins. **The asymmetry is
in the contracts, not in the workflows.**

So `pkg/strategy` mirrors the contract's caps exactly (`TestScanCapsMatchTheContract`
fails if they drift). W2's value is running the decision off-chain against live
state, cross-checking the view, and being the thing that can deliver a report.

## Constraints worth knowing before editing

- **15 chain reads per tick** — see [`docs/READ_BUDGET.md`](../docs/READ_BUDGET.md).
  Per-strategy reads scale with the strategy count; a large registry truncates
  the redemption scan, which is reported as `truncated-scan`, not as a bug.
- **The clock is the block's**, not `runtime.Now()` — [`docs/envelope.md`](../docs/envelope.md).
- **The report carries no amount.** `Decision.Amount` exists for the log and the
  cross-check only; `Report.Build` takes an action and nothing else.

## Verifying a change

```bash
make test
cre workflow simulate strategy-keeper --target local-settings --non-interactive --trigger-index 0
```

See [`docs/LOCAL_FORK.md`](../docs/LOCAL_FORK.md). Watch for `divergence=match`
and non-zero `readsRemaining`.
