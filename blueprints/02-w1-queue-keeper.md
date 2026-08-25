# 02 — W1 queue-keeper (exit-queue automation)

Receiver: `CREQueueExecutor`. Files: `queue-keeper/main.go`,
`queue-keeper/reads.go`, `queue-keeper/write.go`, `pkg/queue/decide.go`,
`pkg/queue/divergence.go`.

## Tick flow

```mermaid
flowchart TB
    T["cron fires"] --> RES["chains.Resolve<br/>registry + queueExecutorAddress"]
    RES --> PRE["readPreamble — 3 reads:<br/>• block timestamp (fired first, awaited last)<br/>• multicall: registry×3 + receiver config×6 + receiver.paused<br/>• multicall: queue facts + protocol pause flags"]

    PRE --> PAU{"paused?<br/>(receiver, Controller,<br/>ExitQueue or AMM)"}
    PAU -- "yes" --> SKIP["skip scan entirely —<br/>every action would revert"]
    PAU -- "no" --> BAL["read Controller ETH balance (1 read)"]

    BAL --> P1["Phase 1 — batch scan:<br/>batchInfo + unprocessedUsersCount per batch,<br/>chunked multicalls, cursor → currentBatchID<br/>(capped at maxBatchScan, default 250)"]
    P1 -- "budget runs out" --> TR["mark ScanTruncatedAt —<br/>scan degrades, tick does NOT abort"]
    P1 -- "done" --> P2
    TR --> P2["Phase 2/3 — users + requestInfo<br/>for non-skippable candidates<br/>until one is affordable<br/>(expired skipped; over-budget<br/>heads continue)"]
    P2 --> DEC
    SKIP --> DEC

    DEC["queue.Decide(state)<br/>(pure decision — see below)"]
    DEC --> XC["cross-check: queueUpkeepStatus() (1 read)<br/>→ queue.Classify"]
    XC --> OUT{"action?"}

    OUT -- "None" --> R0["Result{action: none}"]
    OUT -- "PriceBatch / ProcessRequests /<br/>AdvanceCursor" --> BUILD["buildReport:<br/>sequence = lastSequence + 1 (from this tick's read)<br/>observedAt = observed block timestamp"]
    BUILD --> VAL["envelope.Validate vs live receiver state"]
    VAL -- "reject" --> ERR["error: refuse to emit"]
    VAL -- "ok" --> SH{"shadowMode?"}
    SH -- "yes" --> LOG["log report, no write"]
    SH -- "no" --> W["writeReport → crewrite.Write"]
    W --> OK["Result{wrote, txHash}"]
```

## Decision tree — `queue.Decide` (pkg/queue/decide.go)

Mirrors `CREQueueExecutor.queueUpkeepStatus` branch order, **except** the scan
is unbounded (up to 250 batches) where the on-chain view stops at 25. That is
W1's entire reason to exist: `_processReport` validates a `ProcessRequests`
claim per batch with no scan window.

```mermaid
flowchart TB
    P{"State.Paused?"} -- "yes" --> N1["None — protocol halted"]
    P -- "no" --> FC["fullCursor = PeekAdvancedCursor(0)<br/>(walk past skippable batches, unbounded)"]

    FC --> LOOP{"for id in fullCursor → currentBatchID:<br/>skippable? affordable prefix &gt; 0?"}
    LOOP -- "found oldest batch<br/>with affordable work" --> PR["ProcessRequests(batchId, endIndex)<br/>ScannedBeyondWindow = id ≥ windowEnd"]
    LOOP -- "nothing affordable" --> AGE

    AGE{"scan truncated?"}
    AGE -- "yes" --> CUR
    AGE -- "no" --> AGE2{"current batch unprocessed AND<br/>now - createdAt ≥ minBatchAge?"}
    AGE2 -- "yes" --> PB["PriceBatch(currentBatchId)"]
    AGE2 -- "no" --> CUR

    CUR{"boundedCursor =<br/>PeekAdvancedCursor(MaxBatchScan=25)<br/>&gt; stored cursor?"}
    CUR -- "yes" --> AC["AdvanceCursor(boundedCursor)<br/>claim capped at cursor + 25 — the receiver's<br/>bounded walk cannot reach further"]
    CUR -- "no" --> NN["None"]

    style PR fill:#e8f5e9
    style PB fill:#fff8e1
    style AC fill:#ede7f6
```

## The three actions and what the receiver does with them

```mermaid
flowchart LR
    subgraph W1
        A1["PriceBatch(batchId)"]
        A2["ProcessRequests(batchId, endIndex)"]
        A3["AdvanceCursor(batchId)"]
    end
    subgraph QX["CREQueueExecutor._processReport"]
        B1["priceBatch() on Controller:<br/>locks the batch's final EVE price"]
        B2["re-derives affordability per batch<br/>(no scan window!) and processes<br/>the claimed prefix [0, endIndex)"]
        B3["advances cursor with its own<br/>bounded walk (≤25 batches)"]
    end
    A1 --> B1
    A2 --> B2
    A3 --> B3
```

## Affordability walk — `AffordableRequests` (must match the contract)

```mermaid
flowchart TB
    S["start at request 0, cumulative = 0,<br/>limit = min(unprocessedCount,<br/>maxUsersPerUpkeep, len(requests))"] --> L{"more requests<br/>in prefix?"}
    L -- "yes" --> C["cost = RequestCost(batch.finalEvePrice, request)"]
    C --> SL{"price fell below user's<br/>tolerance? (slippage)"}
    SL -- "yes" --> Z["cost = 0 — request closes,<br/>consumes a slot, no ETH"]
    SL -- "no" --> K["cost = tokensToBurn × finalEvePrice / 1e18<br/>(truncating, solmath.ConvertAssets)"]
    Z --> ACC
    K --> ACC{"cumulative + cost<br/>≤ controller balance?"}
    ACC -- "yes" --> INC["count++, next request"]
    INC --> L
    ACC -- "no" --> BR["BREAK — not skip.<br/>The contract stops at the first request<br/>that overruns; it does not fit<br/>cheaper ones behind it."]
    L -- "no" --> E["endIndex = count"]
    style BR fill:#ffebee
```

## Divergence classification — `queue.Classify`

Shadow mode's exit criterion is "zero `bug`-class divergences over 7 days"
(issue #5):

| Class | Meaning | Action |
| --- | --- | --- |
| `match` | Workflow and `queueUpkeepStatus()` agree | log info |
| `intended-improvement` | Workflow found work beyond the view's two-window reach (`OnChainScanWindowEnd`), or claimed a valid shorter prefix — the receiver accepts any prefix | log info, expected |
| `truncated-scan` | Read budget stopped the process walk short, so a missing `ProcessRequests` or a skipped `PriceBatch` is the workflow refusing to guess | log info, explained |
| `bug` | Anything else — off-chain model or read layer is wrong | log **error**, must stay at zero |

```mermaid
flowchart LR
    WF["W1 decision"] --- CL["Classify"] --- OC["queueUpkeepStatus()"]
    CL -- "same action + batch + prefix" --> M["match"]
    CL -- "W1 deeper / shorter prefix,<br/>within receiver-acceptable rules" --> I["intended-improvement"]
    CL -- "scan truncated, actions differ" --> T["truncated-scan"]
    CL -- "anything else" --> B["bug → alert"]
    style B fill:#ffebee
    style M fill:#e8f5e9
    style I fill:#fff8e1
    style T fill:#fff8e1
```

## Batch lifecycle context (what W1 is automating)

```mermaid
stateDiagram-v2
    [*] --> Open: user exit() joins<br/>current batch
    Open --> Priced: PriceBatch<br/>(after minBatchAge)
    Priced --> Settling: ProcessRequests<br/>(affordable prefix)
    Settling --> Settling: more ProcessRequests
    Settling --> Done: unprocessedCount = 0
    Priced --> Expired: pricedAt + MAX_BATCH_PROCESSING_TIME<br/>(escape hatch)
    Expired --> Skipped: AdvanceCursor —<br/>users close their own requests
    Done --> Skipped: AdvanceCursor
    Skipped --> [*]
```
