# 01 — System overview

How the components in this repo relate to Gelato and the EverStrat contracts.
Files: `web3-functions/queue-keeper/`, `freeze-watch/`, `pkg/*`, and the
executors in
[`everstrat-xyz/contracts`](https://github.com/everstrat-xyz/contracts).

W1/W2 moved off Chainlink CRE when Automation was sunset and CRE turned out
not to be permissionless. W4 (freeze-watch) still runs on CRE pending its own
migration (deferred).

```mermaid
flowchart TB
    subgraph GELATO["Gelato Network"]
        direction TB
        T1["time-based trigger"]
        T2["resolver poll<br/>(every N min)"]

        subgraph W1["W1 — queue-keeper (TypeScript Web3 Function)"]
        end
    end

    subgraph CRE["Chainlink CRE (W4 only, deferred)"]
        W4["W4 — freeze-watch (no writes)"]
    end

    subgraph CHAIN["EVM chain"]
        MC["Multicall3<br/>0xcA11...CA11"]

        subgraph PROTOCOL["EverStrat protocol"]
            REG["Registry"]
            CTRL["Controller"]
            EQ["ExitQueue"]
            AMM["AMM"]
            SM["StrategyManager"]
        end

        subgraph EXEC["Keeper executors (caller-allowlisted)"]
            QX["QueueKeeperExecutor<br/>checker() + perform()"]
            SX["StrategyKeeperExecutor<br/>checker() + perform()"]
        end
    end

    WEBHOOK["Ops webhook<br/>(secret ALERT_WEBHOOK_URL)"]

    T1 -- fires --> W1
    T2 -- "calls checker()" --> SX

    W1 -- "reads: batchInfo, requestInfo,<br/>balances, pause flags<br/>(full scan, no read cap)" --> MC
    W1 -- "cross-check:<br/>queueUpkeepStatus" --> MC
    W4 -- "reads: pause flags, feed ages,<br/>executor liveness<br/>(≤15 per tick, via Multicall3)" --> MC
    MC --- REG & CTRL & EQ & AMM & SM

    W1 -- "submits perform(action, params)<br/>from the dedicated proxy" --> QX
    SX -- "canExec → Gelato submits<br/>perform(action) from the proxy" --> SX

    QX -- "re-validates, then calls<br/>(KEEPER_ROLE)" --> CTRL
    SX -- "re-validates, then calls<br/>(KEEPER_ROLE)" --> CTRL

    W4 -- "alert digest (HTTP POST,<br/>consensus-agreed)" --> WEBHOOK

    style W4 fill:#e8f0fe,stroke:#5c6bc0
    style W1 fill:#e8f5e9,stroke:#43a047
    style SX fill:#fff8e1,stroke:#ffb300
```

## Why W1 is off-chain and W2 is not

The split is forced by the contracts, not a preference:

- `QueueKeeperExecutor._processReport` validates a `ProcessRequests` claim
  **per batch with no scan window**, so a Web3 Function scanning past the
  on-chain view's 25-batch `MAX_BATCH_SCAN` finds work the resolver
  structurally cannot — and the executor accepts it. That depth is W1's whole
  value.
- `StrategyKeeperExecutor._processReport` re-derives every quantity with the
  **same bounded helpers** its view uses, so a "truer" off-chain number would
  be rejected on arrival. Its own `checker()` is therefore exactly as good as
  any off-chain decider could be — so that is what Gelato polls.

## The W1 tick shape

`web3-functions/queue-keeper/index.ts`:

```mermaid
flowchart TB
    T["trigger fires"] --> R["resolve Controller / ExitQueue / AMM<br/>from the Registry (one read set)"]
    R --> P["read pause flags,<br/>cursor, knobs, block timestamp"]
    P -- "anything paused" --> NONE1["canExec: false<br/>(every action would revert)"]
    P --> S["full scan cursor → current:<br/>batchInfo + unprocessedUsersCount,<br/>then user lists + requestInfo for candidates"]
    S --> D["decide(state)<br/>(pure, src/decide.ts)"]
    D --> X["cross-check vs queueUpkeepStatus()<br/>(src/divergence.ts)"]
    X -- "class = bug" --> EL["log Error — must stay at zero"]
    X -- "match / intended-improvement /<br/>truncated-scan" --> IL["log Info"]
    EL --> NA{"action == None?"}
    IL --> NA
    NA -- "yes" --> NONE2["canExec: false, with reason"]
    NA -- "no" --> E["encode perform calldata<br/>(src/params.ts — ids and indices only)"]
    E --> OUT["canExec: true, callData"]
```

One clock, deliberately: `state.now` is the observed block's timestamp, never
`Date.now()` — batch ages are compared against `createdAt` values recorded
from `block.timestamp`, and a wall clock even a second ahead of the chain
would skew every `minBatchAge` check.
