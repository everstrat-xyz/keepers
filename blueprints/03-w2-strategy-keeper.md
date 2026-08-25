# 03 — W2 strategy-keeper (strategy automation)

Receiver: `CREStrategyExecutor`. Files: `strategy-keeper/main.go`,
`strategy-keeper/reads.go`, `pkg/strategy/decide.go`, `pkg/strategy/strategy.go`.

## Tick flow

```mermaid
flowchart TB
    T["cron fires"] --> RES["chains.Resolve<br/>registry + strategyExecutorAddress"]
    RES --> PRE["readPreamble — 3 reads:<br/>• block timestamp<br/>• multicall: registry×5 + receiver thresholds + paused<br/>• multicall: currentBatchId (exclusive bound),<br/>MAX_BATCH_PROCESSING_TIME, queue cursor,<br/>AMM float, strategy list, fee bps,<br/>Controller/StrategyManager paused"]

    PRE --> PAU{"paused?<br/>(receiver, Controller,<br/>StrategyManager)"}
    PAU -- "yes" --> SKIP["skip reads — None"]
    PAU -- "no" --> BAL["Controller ETH balance (1 read)"]

    BAL --> STR["readStrategies — per strategy ×7 sub-calls:<br/>paused, isHealthy, maxDeposit, maxWithdrawal,<br/>isStrategyInDepositCooldown, depositWeight,<br/>pendingPerformanceFeeInETH<br/>(chunked multicall)"]

    STR --> NDS["readPendingNeeds:<br/>[cursor, currentBatchId) headers, ≤25<br/>users only if NeedsCostScan<br/>(priced, unexpired, unprocessed &gt; 0)<br/>≤50 users/batch — SAME caps as contract"]

    NDS --> DEC["strategy.Decide(state) — see below"]
    DEC --> XC["cross-check: strategyUpkeepStatus() (1 read)<br/>→ strategy.Classify"]
    XC --> OUT{"action?"}
    OUT -- "None" --> R0["Result"]
    OUT -- "action" --> BUILD["buildReport: action only —<br/>Report.Build takes NO amount.<br/>Receiver recomputes every quantity."]
    BUILD --> VAL["envelope.Validate"]
    VAL --> SH{"shadowMode?"}
    SH -- "yes" --> LOG["log, no write"]
    SH -- "no" --> W["writeReport → crewrite.Write"]
```

`currentBatchId` is a bound, not a scan target. The current batch is unpriced
cancellable equity until `priceBatch` (contracts PR #43, M-11); costing it at
the AMM base price would propose `WithdrawShortfall` the receiver recomputes as
none. There is no `eveBasePriceInETH` read.

## Decision tree — `strategy.Decide` (pkg/strategy/decide.go)

Mirrors `CREStrategyExecutor.strategyUpkeepStatus` **branch for branch, in
order**. First match wins. Enum ordinals are a different order
(`ProvideExitLiquidity` is 6); `Priority` in `pkg/strategy/strategy.go` is this
tree.

```mermaid
flowchart TB
    P{"State.Paused?"} -- "yes" --> N0["None"]
    P -- "no" --> A1{"any strategy<br/>not paused AND unhealthy?"}
    A1 -- "yes" --> R1["1. Rebalance<br/>(pull funds out of the sick strategy)"]
    A1 -- "no" --> A2

    A2{"needsETH &gt; controllerBalance<br/>AND shortfall ≥ minWithdrawETH<br/>AND Σ maxWithdrawal &gt; 0?"}
    A2 -- "yes" --> R2["2. WithdrawShortfall<br/>(amount = needs − balance, diagnostic only)"]
    A2 -- "no" --> A3

    A3{"topUp = min(target − float,<br/>idleExcess) ≥ minExitLiquidityTopUpETH<br/>AND &gt; 0?"}
    A3 -- "yes" --> R3["3. ProvideExitLiquidity<br/>(refill the AMM exit float)"]
    A3 -- "no" --> A4

    A4{"idleExcess = balance −<br/>(reserve + needs) ≥ minDepositETH<br/>AND some strategy has capacity<br/>(healthy, off cooldown, maxDeposit &gt; 0,<br/>depositWeight &gt; 0)?"}
    A4 -- "yes" --> R4["4. DepositExcess<br/>(put idle ETH to work)"]
    A4 -- "no" --> A5

    A5{"Σ pendingPerformanceFee ≥ minHarvestETH<br/>AND fee &gt; 0?<br/>(feeBps = 0 short-circuits the sum to 0)"}
    A5 -- "yes" --> R5["5. HarvestPerformanceFees"]
    A5 -- "no" --> A6

    A6{"syncInterval ≠ 0 AND strategies exist AND<br/>now ≥ lastSyncAt AND<br/>now − lastSyncAt ≥ syncInterval?"}
    A6 -- "yes" --> R6["6. Sync<br/>(periodic accounting refresh)"]
    A6 -- "no" --> N1["None"]

    style R1 fill:#ffcdd2
    style R2 fill:#ffebee
    style R3 fill:#fff8e1
    style R4 fill:#e8f5e9
    style R5 fill:#ede7f6
    style R6 fill:#e1f5fe
```

Where the money-model helpers come from (`decide.go`):

- `idleExcess(balance, needs)` = `max(0, balance − (controllerReserveETH + needs))`
- `exitLiquidityTopUp` = `min(exitLiquidityTargetETH − ammFreeBalance, idleExcess)`, 0 if float ≥ target
- `depositCapacityAvailable` requires `depositWeight > 0` (R4-M-04): all-zero
  weights are registered-but-unfunded; the StrategyManager refunds the Controller
- `PendingRedemptionNeedsETH` — see below

## Why W2 must NOT scan deeper than the contract

The single most important asymmetry in this repo (see the long comment on
`strategy.Decide`):

```mermaid
flowchart TB
    subgraph W1C["W1 / CREQueueExecutor"]
        V1["_processReport validates ProcessRequests<br/>PER BATCH — no scan window.<br/>→ scanning deeper off-chain is a genuine win"]
    end
    subgraph W2C["W2 / CREStrategyExecutor"]
        V2["_processReport re-derives every quantity<br/>with the SAME bounded helpers as the view<br/>(MAX_BATCH_SCAN=25, MAX_USERS_COST_SCAN=50).<br/>→ a 'truer' shortfall reverts EVERY time:<br/>KeeperExecutorNoUpkeepNeeded"]
    end
    CONCL["W1 scans deep (budget-capped).<br/>W2 mirrors the contract EXACTLY:<br/>MaxBatchScan=25, MaxUsersCostScan=50,<br/>pinned by TestScanCapsMatchTheContract."]
    V1 --> CONCL
    V2 --> CONCL
    style V2 fill:#ffebee
    style CONCL fill:#fff8e1
```

If the 15-read budget cannot cover those contract caps, W2 does **not**
widen or invent a deeper figure. It truncates, understates `NeedsETH`, and
classifies the disagreement as `truncated-scan`.

## `PendingRedemptionNeedsETH` — what pending redemptions cost

Walk is `[cursor, currentBatchId)`, `id < cursor + 25`. Exclusive of the
current batch. Expired / empty / unpriced batches contribute 0 without a user
list (`NeedsCostScan`).

```mermaid
flowchart TB
    S["needs = 0"] --> L1{"for id in [cursor, currentBatchId),<br/>capped at cursor + 25:"}
    L1 -- "batch" --> G{"batch in map?<br/>(missing = unreadable → skip)"}
    G -- "yes" --> H{"NeedsCostScan?<br/>CanBeProcessed AND unprocessed &gt; 0<br/>AND not past escape hatch (strict &gt;)?"}
    H -- "yes" --> W["for first ≤50 users:<br/>slipped price? skip (0 cost) :<br/>needs += tokensToBurn × finalEvePrice / 1e18"]
    H -- "no" --> NX["batch contributes 0"]
    W --> L1
    NX --> L1
    L1 -- "done" --> E["return needs"]

    style E fill:#e8f5e9
```

The current unpriced batch is **not** added at the AMM base price. That was
M-11: until `priceBatch` the queued EVE is cancellable equity
(`liveRedemptionOffsets` is zero). After `priceBatch`, `currentBatchId`
increments and the batch sits in the window, costed at `finalEvePrice`.

Note the deliberate difference from W1's affordability walk: this sums every
request with **no balance budget and no early break** — it asks "what do
pending redemptions cost?", not "what can we afford right now?".

## Divergence classification — `strategy.Classify`

Not W1's scheme. W2 has no "found work the view could not see" class, because
the receiver re-derives with the same caps. Classes are in
`pkg/strategy/divergence.go`:

| Class | Meaning | Action |
| --- | --- | --- |
| `match` | Same action and amount | log info |
| `amount-only` | Same action, different amount — the report carries no amount; the receiver recomputes | log info |
| `truncated-scan` | `ScanTruncated`: budget could not cover the bounded walk, so `NeedsETH` understates and the action may disagree | log info, explained |
| `bug` | Unexplained action disagreement — the report would revert | log **error**, must stay at zero |
