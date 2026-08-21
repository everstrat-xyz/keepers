# 01 — System overview

How the three workflows in this repo relate to the Chainlink DON and the
EverStrat contracts. Files: `queue-keeper/`, `strategy-keeper/`, `freeze-watch/`,
`pkg/*`, and the receivers in
[`everstrat-xyz/contracts`](https://github.com/everstrat-xyz/contracts).

```mermaid
flowchart TB
    subgraph DON["Chainlink Runtime Environment (DON)"]
        direction TB
        CRON["cron trigger<br/>(per-workflow schedule)"]

        subgraph W1["W1 — queue-keeper"]
        end
        subgraph W2["W2 — strategy-keeper"]
        end
        subgraph W4["W4 — freeze-watch (no writes)"]
        end

        CRON --> W1
        CRON --> W2
        CRON --> W4
    end

    subgraph CHAIN["EVM chain (Sepolia until cutover, issue #6)"]
        direction TB
        MC["Multicall3<br/>0xcA11...CA11"]

        subgraph PROTOCOL["EverStrat protocol"]
            REG["Registry"]
            CTRL["Controller"]
            EQ["ExitQueue"]
            AMM["AMM"]
            SM["StrategyManager"]
        end

        subgraph RECEIVERS["CRE receivers (hold KEEPER_ROLE)"]
            QX["CREQueueExecutor"]
            SX["CREStrategyExecutor"]
        end

        FWD["KeystoneForwarder<br/>(verifies DON report)"]
    end

    WEBHOOK["Ops webhook<br/>(secret ALERT_WEBHOOK_URL)"]

    W1 -- "reads: batchInfo, requestInfo,<br/>balances, pause flags<br/>(≤15 per tick, via Multicall3)" --> MC
    W2 -- "reads: strategies, AMM float,<br/>redemption needs<br/>(≤15 per tick, via Multicall3)" --> MC
    W4 -- "reads: pause flags, feed ages,<br/>receiver liveness" --> MC
    MC --- REG & CTRL & EQ & AMM & SM

    W1 -- "DON-signed report:<br/>PriceBatch / ProcessRequests / AdvanceCursor" --> FWD
    W2 -- "DON-signed report:<br/>Rebalance / WithdrawShortfall / ... / Sync" --> FWD
    FWD --> QX & SX

    QX -- "calls (KEEPER_ROLE)" --> CTRL
    SX -- "calls (KEEPER_ROLE)" --> CTRL

    W4 -- "alert digest (HTTP POST,<br/>consensus-agreed)" --> WEBHOOK

    style W4 fill:#e8f0fe,stroke:#5c6bc0
    style W1 fill:#e8f5e9,stroke:#43a047
    style W2 fill:#fff8e1,stroke:#ffb300
    style FWD fill:#fce4ec,stroke:#e53935
```

## The shared tick shape (W1 and W2)

Both keepers run the same skeleton every cron fire — see `queue-keeper/main.go`
and `strategy-keeper/main.go`:

```mermaid
flowchart TB
    T["cron fires"] --> R["chains.Resolve config<br/>(registry + receiver address)"]
    R -- "unresolved" --> S{"shadowMode?"}
    S -- "yes" --> SW["log warn, Result{bound: false},<br/>tick succeeds — must not wedge"]
    S -- "no" --> FAIL["return error —<br/>live keeper misconfigured"]
    R -- "ok" --> P["readPreamble<br/>(registry lookups, receiver config,<br/>pause flags, block timestamp)"]
    P --> CS{"receiver.CHAIN_SELECTOR<br/>== config chain?"}
    CS -- "no" --> FAIL2["error: wrong receiver or wrong chain"]
    CS -- "yes" --> ST["read state<br/>(budgeted scan)"]
    ST --> D["Decide(state)<br/>(pure, pkg/queue / pkg/strategy)"]
    D --> X["cross-check vs on-chain view<br/>(queueUpkeepStatus / strategyUpkeepStatus)"]
    X -- "divergence = bug" --> EL["log Error — must stay at zero<br/>(shadow-mode exit criterion, issue #5)"]
    X -- "match / intended-improvement / truncated-scan" --> IL["log Info"]
    EL --> NA{"action == None?"}
    IL --> NA
    NA -- "yes" --> DONE["Result, done"]
    NA -- "no" --> BR["buildReport:<br/>sequence = receiver.lastSequence + 1,<br/>observedAt = observed block timestamp"]
    BR --> V["envelope.Validate vs live receiver state"]
    V -- "would be rejected" --> FAIL3["error: refuse to emit<br/>(saves a wasted delivery)"]
    V -- "ok" --> SM2{"shadowMode?"}
    SM2 -- "yes" --> SH["log report, do NOT write<br/>(on until Sepolia cutover, issue #6)"]
    SM2 -- "no" --> WR["crewrite.Write<br/>(GenerateReport → WriteReport)"]
    WR -- "tx landed, receiver accepted" --> OK["Result{wrote: true, txHash}"]
    WR -- "tx landed, receiver reverted<br/>(KeeperExecutorNoUpkeepNeeded)" --> REV["warn + txHash — expected:<br/>state moved between observe and deliver"]
    WR -- "tx failed" --> FAIL4["error"]
```

Two clocks, deliberately:

| Time source | Used for |
| --- | --- |
| `evmread.BlockTimestamp` (observed block) | `observedAt`, batch ages, sync age — anything compared against contract state |
| `runtime.Now()` (DON wall clock) | only `Validate`'s delivery-time argument |

The receiver rejects `observedAt > block.timestamp` with zero tolerance, so a
wall clock even one second ahead of the chain would fail *every* report.
