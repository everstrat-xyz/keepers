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
    R1["1. preamble<br/>block timestamp +<br/>registry×3 + receiver×7<br/>(multicall)"] --> R2["2. round-2 multicall<br/>queue facts + pause flags"]
    R2 --> R3["3. Controller balance"]
    R3 --> R4["4. queueUpkeepStatus<br/>(reserved)"]
    R4 --> R5["N. batchInfo + counts<br/>(chunked)"]
    R5 --> R6["M. unprocessedUsers<br/>(oldest candidate only)"]
    R6 --> R7["K. requestInfo<br/>(chunked)"]
```

Budget bookkeeping (`queue-keeper/reads.go`):

- `reservedReads = 2` held back for the cross-check view + write-path margin
- Phase 1 keeps `Remaining() − 2` for the user/request phases — batches found
  without the reads to evaluate them are worse than batches not found
- Users are read **only for the oldest processable candidate**: `Decide`
  picks the first affordable batch, so reading further candidates spends
  budget on batches that cannot be chosen this tick
- Healthy small queues finish with 5–8 reads spare

## W2's read plan

```mermaid
flowchart LR
    S1["1. block timestamp"] --> S2["2. registry×5 +<br/>receiver thresholds (multicall)"]
    S2 --> S3["3. queue cursor, AMM float + price,<br/>strategy list, fee bps (multicall)"]
    S3 --> S4["4. Controller balance"]
    S4 --> S5["5. per-strategy ×6 sub-calls<br/>(multicall, chunked)"]
    S5 --> S6["M. bounded redemption scan<br/>(≤25 batches, ≤50 users —<br/>mirrors the contract)"]
    S6 --> S7["strategyUpkeepStatus<br/>(reserved)"]
```

Unlike W1, W2's scan width is **not** budget-driven — it is contract-driven
(`MaxBatchScan=25`, `MaxUsersCostScan=50`). If the budget cannot cover the
bounded scan, the tick records `ScanTruncated=true` and `NeedsETH` may
understate — surfaced via the divergence classification rather than silently
changing the decision.

## What a pinned budget test looks for

`TestReadPlanFitsBudget` (in each read-layer test file) asserts the fixed
preamble plus cross-check still leaves reads for the scan — so adding one
read to a preamble without adjusting the test fails CI, not production.
