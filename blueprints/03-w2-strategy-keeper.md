# 03 — W2 strategy-keeper (strategy automation)

Receiver: `CREStrategyExecutor`. Files: `strategy-keeper/main.go`,
`strategy-keeper/reads.go`, `pkg/strategy/decide.go`, `pkg/strategy/strategy.go`.

## Tick flow

```mermaid
flowchart TB
    T["cron fires"] --> RES["chains.Resolve<br/>registry + strategyExecutorAddress"]
    RES --> PRE["readPreamble — 3 reads:<br/>• block timestamp<br/>• multicall: registry×5 + receiver thresholds + paused<br/>• multicall: queue cursor (nextLiveBatchIdToProcess),<br/>AMM float + base price, strategy list, fee bps,<br/>Controller/StrategyManager paused"]

    PRE --> PAU{"paused?<br/>(receiver, Controller,<br/>StrategyManager)"}
    PAU -- "yes" --> SKIP["skip reads — None"]
    PAU -- "no" --> BAL["Controller ETH balance (1 read)"]

    BAL --> STR["readStrategies — per strategy ×6 sub-calls:<br/>paused, isHealthy, maxDeposit, maxWithdrawal,<br/>isStrategyInDepositCooldown, pendingPerformanceFeeInETH<br/>(one multicall, chunked)"]

    STR --> NDS["readPendingNeeds:<br/>reproduce _pendingRedemptionNeedsETH from raw<br/>queue reads — capped at MaxBatchScan=25 batches,<br/>MaxUsersCostScan=50 users (SAME as contract)"]

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

## Decision tree — `strategy.Decide` (pkg/strategy/decide.go)

Mirrors `CREStrategyExecutor.strategyUpkeepStatus` **branch for branch, in
order**. First match wins.

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

    A4{"idleExcess = balance −<br/>(reserve + needs) ≥ minDepositETH<br/>AND some strategy has capacity<br/>(healthy, off cooldown, maxDeposit &gt; 0)?"}
    A4 -- "yes" --> R4["4. DepositExcess<br/>(put idle ETH to work)"]
    A4 -- "no" --> A5

    A5{"Σ pendingPerformanceFee ≥ minHarvestETH<br/>AND feeBps ≠ 0?"}
    A5 -- "yes" --> R5["5. HarvestPerformanceFees"]
    A5 -- "no" --> A6

    A6{"syncInterval ≠ 0 AND strategies exist AND<br/>now − lastSyncAt ≥ syncInterval?"}
    A6 -- "yes" --> R6["6. Sync<br/>(periodic accounting refresh)"]
    A6 -- "no" --> N1["None"]

    style R1 fill:#ffcdd2
    style R2 fill:#ffebee
    style R3 fill:#fff8e1
    style R4 fill:#e8f5e9
    style R5 fill:#ede7f6
    style R6 fill:#e1f5fe
```

Where the money-model helpers come from (`strategy.go`):

- `idleExcess(balance, needs)` = `max(0, balance − (controllerReserveETH + needs))`
- `exitLiquidityTopUp` = `min(exitLiquidityTargetETH − ammFreeBalance, idleExcess)`, 0 if float ≥ target
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
    CONCL["W1 mirrors nothing — it scans deep.<br/>W2 mirrors the contract EXACTLY:<br/>MaxBatchScan=25, MaxUsersCostScan=50,<br/>pinned by TestScanCapsMatchTheContract."]
    V1 --> CONCL
    V2 --> CONCL
    style V2 fill:#ffebee
    style CONCL fill:#fff8e1
```

## `PendingRedemptionNeedsETH` — what pending redemptions cost

```mermaid
flowchart TB
    S["needs = 0"] --> L1{"for id in cursor → currentBatchID,<br/>capped at cursor + 25:"}
    L1 -- "batch" --> G{"batch in map?<br/>(missing = unreadable → skip)"}
    G -- "yes" --> H{"CanBeProcessed AND<br/>pricedAt + MAX_BATCH_PROCESSING<br/>not passed AND unprocessed &gt; 0?"}
    H -- "yes" --> W["for first ≤50 users:<br/>slipped price? skip (0 cost) :<br/>needs += tokensToBurn × finalEvePrice / 1e18"]
    H -- "no" --> NX["batch contributes 0"]
    W --> L1
    NX --> L1
    L1 -- "done" --> CB{"current batch has<br/>totalTokensToBurn &gt; 0?"}
    CB -- "yes" --> CBN["needs += totalTokensToBurn ×<br/>AMM base price / 1e18<br/>(regardless of scan window)"]
    CB -- "no" --> E["return needs"]
    CBN --> E

    style E fill:#e8f5e9
```

Note the deliberate difference from W1's affordability walk: this sums every
request with **no balance budget and no early break** — it asks "what do
pending redemptions cost?", not "what can we afford right now?".

## Divergence classification — `strategy.Classify`

Same three-class scheme as W1 (`match` / `intended-improvement` / `bug`), but
for W2 there is no scan-window excuse: since the workflow mirrors the
contract's helpers exactly, the expected class is almost always `match`. Any
`bug` means the read layer or the transcription drifted — see
`pkg/strategy/divergence.go`.
