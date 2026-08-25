# The chain-read budget

CRE allows a workflow **15 contract reads per execution**
(`ChainRead.CallLimit`), with a **5 kB** cap on each read's response payload
(`ChainRead.PayloadSizeLimit`). Exceeding either aborts the whole execution with
`Public:User:LimitExceeded` — there is no partial result.

Confirm the current values for your tenant:

```bash
cre workflow limits export | jq '.ChainRead'
```

## Why this shapes W1 and W2

[Issue #3](https://github.com/everstrat-xyz/keepers/issues/3) specified a
**full** off-chain scan of the exit queue — "no 25-batch window" — as W1's
advantage over the gas-bounded on-chain `queueUpkeepStatus` view. At one read
per contract call that is not achievable: the address resolution and receiver
config alone need ~13 reads before touching a single batch.

The resolution is **Multicall3 aggregation**. `aggregate3` packs many sub-calls
into one `eth_call`, which is one chain read no matter how many sub-calls it
carries. The canonical deployment
(`0xcA11bde05977b3631167028862bE2a173976CA11`) is live on both Sepolia and
mainnet.

That converts the binding constraint from *call count* to *response size*:

| Return shape | Bytes per result (framed) | Results per 5 kB read |
| --- | --- | --- |
| `batchInfo` / `requestInfo` (160 B) | 320 | 16 |
| `unprocessedUsersCount` (32 B) | 192 | 26 |

## W1's plan

| Reads | Purpose |
| --- | --- |
| 1 | block timestamp (the clock — see [envelope.md](envelope.md)) |
| 1 | multicall: Registry×3 + receiver config×6 + receiver `paused` |
| 1 | multicall: `currentBatchId`, `MAX_BATCH_PROCESSING_TIME`, 3× `paused` |
| 1 | Controller ETH balance |
| ~10 | multicalls: batch scan, user lists, request detail |
| 1 | `queueUpkeepStatus` cross-check (reserved) |

That reaches roughly **64 batches per tick** against the on-chain view's 25 —
pinned by `TestReadPlanFitsBudget` in `pkg/evmread`, which fails if a change to
the plan erodes the advantage. The current (unpriced) batch **is** in W1's
Phase 1 window: `PriceBatch` needs its age and unprocessed count.

Users are loaded for **non-skippable** priced batches, oldest first, until one
has an affordable prefix. Expired and empty heads skip the user read; an
over-budget in-window head is not skippable, so the next candidate is loaded
rather than stalling.

## W2's plan

| Reads | Purpose |
| --- | --- |
| 1 | block timestamp |
| 1 | multicall: Registry×5 + receiver thresholds + receiver `paused` |
| 1 | multicall: `currentBatchId` (exclusive bound), `MAX_BATCH_PROCESSING_TIME`, queue cursor, AMM `freeBalance`, fee bps, strategy list, 2× `paused` |
| 1 | Controller ETH balance |
| 1+ | per-strategy ×7 (`paused`, `isHealthy`, `maxDeposit`, `maxWithdrawal`, cooldown, `depositWeight`, pending fee), chunked |
| M | `[cursor, currentBatchId)` headers (≤25); `unprocessedUsers` + `requestInfo` only if `NeedsCostScan` |
| 1 | `strategyUpkeepStatus` cross-check |

W2's scan **width** is the contract cap (`MaxBatchScan=25`,
`MaxUsersCostScan=50`), pinned by `TestScanCapsMatchTheContract` — not leftover
budget. The current unpriced batch is not fetched (M-11: cancellable equity,
not a liability). Expired / empty / unpriced in-window batches still get Phase 1
headers so `pricedAt` is known, then Phase 2 skips their users.

There is no `AMM.eveBasePriceInETH` read. Costing the current batch at the live
base price would propose `WithdrawShortfall` the receiver recomputes as none.

## Degrading, not failing

`evmread.Budget` is taken from before each round rather than discovered at the
limit. When the scan runs out it stops and marks the state truncated
(`queue.State.ScanTruncated` / `strategy.State.ScanTruncated`), which the tick
logs as `scanTruncated=true`.

A truncated scan can only cause the workflow to propose **less** work than
exists, never wrong work — `Decide` walks batches oldest-first, so a short scan
is a prefix of a long one. W1 classifies the miss as `truncated-scan` and
**refuses `PriceBatch`** until a later tick can finish the process walk (pricing
would still be accepted on-chain and would grow the live-priced set instead of
settling). W2's understated `NeedsETH` is `truncated-scan` in
`strategy.Classify`. The cost is that "found nothing" becomes ambiguous, which
is why it is logged rather than silently tolerated.

## Rules for new reads

1. **Never call in a loop.** One `Aggregate` per round, chunked with
   `ChunkSubCalls`. Dynamic returns (`unprocessedUsers` address[]) must be
   sized for the worst-case element count — three full 50-user lists in one
   read exceed 5 kB and abort the tick (`TestUnprocessedUsersListsMustBeChunked`).
2. **Take budget before issuing**, and handle refusal by degrading.
3. **Only read what can change the decision.** W1 reads users for non-skippable
   priced batches in Decide order, stopping at the first affordable prefix.
   W2 reads `depositWeight` per strategy because `_depositCapacityAvailable`
   gates on it; queue user lists only for priced, in-window, unexpired batches.
4. **Update `TestReadPlanFitsBudget`** when W1's fixed plan changes, so the
   remaining scan depth stays visible. W2 cap changes go in
   `TestScanCapsMatchTheContract`.
5. **Verify on the fork** ([LOCAL_FORK.md](LOCAL_FORK.md)). Every tick logs
   `readsRemaining`; the limit is only enforced at runtime.
