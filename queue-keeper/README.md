# W1 — queue-keeper

Implemented, running in **shadow mode**: it decides an action every tick and
cross-checks against the on-chain view, but does not `writeReport` until the
Sepolia cutover ([issue #6](https://github.com/everstrat-xyz/keepers/issues/6))
flips `shadowMode` off.

On-chain consumer: `CREQueueExecutor`.

## What a tick does

1. Resolve Controller / ExitQueue / AMM from the **Registry** (never configured
   directly, so a protocol redeploy cannot leave the keeper pointing at a dead
   address).
2. Read the receiver's own state — `lastSequence`, `CHAIN_SELECTOR`,
   `MAX_REPORT_AGE`, `minBatchAge`, `maxUsersPerUpkeep`, pause flags.
3. Scan the exit queue and reproduce `Controller._processRequest`'s
   affordability arithmetic off-chain ([`pkg/queue`](../pkg/queue),
   [`pkg/solmath`](../pkg/solmath)).
4. Pick `PriceBatch` / `ProcessRequests` / `AdvanceCursor` / none, in the same
   priority order as `queueUpkeepStatus`.
5. Cross-check against `queueUpkeepStatus()` and classify the difference as
   `match` / `intended-improvement` / `bug` (issue #5's monitoring signal).
6. Build the Envelope report and validate it against the receiver's live guards.
   In shadow mode it stops here; otherwise it delivers via
   [`pkg/crewrite`](../pkg/crewrite).

## Constraints worth knowing before editing

- **15 chain reads per tick.** Reads batch through Multicall3 and take from an
  explicit budget. See [`docs/READ_BUDGET.md`](../docs/READ_BUDGET.md); the plan
  reaches ~64 batches against the on-chain view's 25.
- **The clock is the block's, not `runtime.Now()`.** Batch ages and `observedAt`
  both come from the observed block — see [`docs/envelope.md`](../docs/envelope.md).
- **`startIndex` is always 0.** The receiver only accepts a prefix of the
  affordable set, so `EncodeProcessRequestsParams` does not take the argument.
- **`AdvanceCursor` is capped** at the receiver's own bounded cursor walk;
  claiming the unbounded cursor reverts.
- **No amounts, ever.** Params carry a batch id and an index range.

## Verifying a change

Unit tests cover the decision engine. The read path is only exercised by the
local fork harness — [`docs/LOCAL_FORK.md`](../docs/LOCAL_FORK.md):

```bash
make test
cre workflow simulate queue-keeper --target local-settings --non-interactive --trigger-index 0
```

Watch for `divergence=match` and a non-zero `readsRemaining` in the tick log.
