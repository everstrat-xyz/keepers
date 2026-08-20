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
| 1 | multicall: Registry lookups + receiver config + receiver `paused` |
| 1 | multicall: `currentBatchId`, `MAX_BATCH_PROCESSING_TIME`, 3× `paused` |
| 1 | Controller ETH balance |
| 1 | `queueUpkeepStatus` cross-check |
| ~10 | multicalls: batch scan, user lists, request detail |

That reaches roughly **64 batches per tick** against the on-chain view's 25 —
pinned by `TestReadPlanFitsBudget` in `pkg/evmread`, which fails if a change to
the plan erodes the advantage.

## Degrading, not failing

`evmread.Budget` is taken from before each round rather than discovered at the
limit. When the scan runs out it stops and marks the state truncated
(`queue.State.ScanTruncated`), which the tick logs as `scanTruncated=true`.

A truncated scan can only cause the workflow to propose **less** work than
exists, never wrong work — `Decide` walks batches oldest-first, so a short scan
is a prefix of a long one. The cost is that "found nothing" becomes ambiguous,
which is why it is logged rather than silently tolerated.

## Rules for new reads

1. **Never call in a loop.** One `Aggregate` per round, chunked with
   `ChunkSubCalls`.
2. **Take budget before issuing**, and handle refusal by degrading.
3. **Only read what can change the decision.** W1 reads the user list for the
   oldest processable batch only, because `Decide` cannot choose any other batch
   in the same tick. W2 reads `depositWeight` per strategy because
   `_depositCapacityAvailable` gates on it, and no longer reads
   `AMM.eveBasePriceInETH` at all — the current batch stopped being a liability
   in contracts PR #43, so nothing in W2's model consumes a live price.
4. **Update `TestReadPlanFitsBudget`** when the fixed plan changes, so the
   remaining scan depth stays visible.
5. **Verify on the fork** ([LOCAL_FORK.md](LOCAL_FORK.md)). Every tick logs
   `readsRemaining`; the limit is only enforced at runtime.
