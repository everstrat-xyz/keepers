# 05 — The read budget

File: `pkg/evmread/multicall.go`, plus the read plans in
`queue-keeper/reads.go` and `strategy-keeper/reads.go`. See also
[`docs/READ_BUDGET.md`](../docs/READ_BUDGET.md).

## The constraint

CRE allows **15 contract reads per execution** (`ChainRead.CallLimit = 15`)
with a **5 kB response cap** per read (`PayloadSizeLimit`). Exceeding the
count aborts the *whole execution* with `Public:User:LimitExceeded` — no
partial result. This was discovered the hard way: the first read layer needed
~16 calls before touching a single batch.

## The two mechanisms that stretch it

```mermaid
flowchart TB
    subgraph MC["Multicall3 (canonical CREATE2 address,<br/>same on every target chain)"]
        IN["n SubCalls"] --> AGG["one aggregate3 read =<br/>one budget unit, regardless of n"]
    end
    subgraph CH["ChunkSubCalls"]
        CIN["calls with large returns<br/>(batchInfo ≈ 160 B each)"] --> SPLIT["split into batches of<br/>⌊5120 / estimatedBytes⌋<br/>results per read"]
    end
    MC --> B["evmread.Budget<br/>Take(n) before every read;<br/>starts at 15 − reserved"]
    CH --> B
    B -- "runs dry mid-scan" --> DG["STOP and mark truncated.<br/>Never abort the tick."]
    style DG fill:#fff8e1
```

Degradation contract: a scan that runs out of budget **truncates and says so**
(`scanTruncated=true` in the tick log, `ScanTruncatedAt` in state). A
truncated scan can only *under-propose* work — never propose wrong work.

## W1's read plan

```mermaid
flowchart LR
    R1["1. block timestamp"] --> R2["2. registry×3 + receiver×6<br/>+ paused (multicall)"]
    R2 --> R3["3. currentBatchId,<br/>MAX_BATCH_PROCESSING_TIME,<br/>3× paused (multicall)"]
    R3 --> R4["4. Controller balance"]
    R4 --> R5["N. batchInfo + counts<br/>cursor → currentBatchID<br/>(current included: PriceBatch)"]
    R5 --> R6["M. unprocessedUsers<br/>(non-skippable candidates<br/>until one is affordable)"]
    R6 --> R7["K. requestInfo<br/>(chunked)"]
    R7 --> R8["queueUpkeepStatus<br/>(reserved)"]
```

Budget bookkeeping (`queue-keeper/reads.go`):

- `reservedReads = 2` held back for the cross-check view + write-path margin
- Phase 1 keeps `Remaining() − 2` for the user/request phases — batches found
  without the reads to evaluate them are worse than batches not found
- Users are read for **non-skippable** priced batches, oldest first, until
  one has an affordable prefix — the same walk as `Decide` /
  `queueUpkeepStatus`. Expired heads skip the user read; an over-budget
  in-window head is not skippable, so the next candidate is loaded rather
  than stalling. Once a batch is affordable, later ids cannot be chosen
  this tick.
- Healthy small queues finish with 5–8 reads spare

## W2's read plan

```mermaid
flowchart LR
    S1["1. block timestamp"] --> S2["2. registry×5 +<br/>receiver thresholds (multicall)"]
    S2 --> S3["3. cursor, currentBatchId bound,<br/>MAX_BATCH_PROCESSING_TIME,<br/>AMM float, strategy list, fee bps,<br/>pause flags (multicall)"]
    S3 --> S4["4. Controller balance"]
    S4 --> S5["5. per-strategy ×7 sub-calls<br/>(incl. depositWeight;<br/>multicall, chunked)"]
    S5 --> S6["M. [cursor, currentBatchId) headers ≤25;<br/>users only if NeedsCostScan<br/>(priced, unexpired, has work);<br/>lists + requestInfo chunked, ≤50 users"]
    S6 --> S7["strategyUpkeepStatus<br/>(reserved)"]
```

Unlike W1, W2's scan width is **not** budget-driven — it is contract-driven
(`MaxBatchScan=25`, `MaxUsersCostScan=50`). Phase 1 walks
`[cursor, currentBatchId)` headers only (the current unpriced batch is not a
liability). Phase 2 loads users only for priced, unexpired batches with work.
If the budget cannot cover the bounded scan, the tick records
`ScanTruncated=true` and `NeedsETH` may understate — surfaced via the
divergence classification rather than silently changing the decision.

## What a pinned budget test looks for

`TestReadPlanFitsBudget` (`pkg/evmread`) is W1 arithmetic: the fixed preamble
plus cross-check must still leave a scan deeper than the on-chain 25-batch
window. Adding a preamble read without updating it fails CI, not production.
W2's width is the contract cap (`TestScanCapsMatchTheContract`), not leftover
budget.
