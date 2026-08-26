# 01 — System overview

How the components in this repo relate to Mimic and the EverStrat contracts.
Files: `mimic-functions/queue-keeper/`, `mimic-functions/strategy-keeper/`,
and the executors in
[`everstrat-xyz/contracts`](https://github.com/everstrat-xyz/contracts).

W1/W2 moved off Chainlink CRE when Automation was sunset and CRE turned out
not to be permissionless (a Gelato interlude ended the same way when Gelato
sunset its automation). W4 (freeze-watch) was a read-only CRE workflow and has
been removed rather than carried unmigrated — see the README.

```mermaid
flowchart TB
    subgraph MIMIC["Mimic Network"]
        direction TB
        T1["time-based trigger"]
        T2["time-based trigger<br/>(every N min)"]

        subgraph W1["W1 — queue-keeper (Mimic function, deep scan)"]
        end

        subgraph W2["W2 — strategy-keeper (Mimic relay function)"]
        end
    end

    subgraph CHAIN["EVM chain"]
        subgraph PROTOCOL["EverStrat protocol"]
            CTRL["Controller"]
            EQ["ExitQueue"]
            AMM["AMM"]
        end

        subgraph EXEC["Keeper executors (caller-allowlisted)"]
            QX["QueueKeeperExecutor<br/>checker() + perform()"]
            SX["StrategyKeeperExecutor<br/>checker() + perform()"]
        end
    end

    T1 -- fires --> W1
    T2 -- fires --> W2

    W1 -- "oracle EvmCall reads: batchInfo, requestInfo,<br/>balances, pause flags<br/>(full scan, config-bounded)" --> EQ
    W1 -- "pause fan-out" --> CTRL & AMM
    W1 -- "cross-check:<br/>queueUpkeepStatus" --> QX
    W2 -- "oracle EvmCall read:<br/>checker()" --> SX

    W1 -- "EvmCall intent: perform(action, params)<br/>from the smart-account signer" --> QX
    W2 -- "EvmCall intent: execPayload verbatim,<br/>from the smart-account signer" --> SX

    QX -- "re-validates, then calls<br/>(KEEPER_ROLE)" --> CTRL
    SX -- "re-validates, then calls<br/>(KEEPER_ROLE)" --> CTRL

    style W1 fill:#e8f5e9,stroke:#43a047
    style W2 fill:#fff8e1,stroke:#ffb300
```

## Why W1 decides off-chain and W2 does not

The split is forced by the contracts, not a preference:

- `QueueKeeperExecutor._processReport` validates a `ProcessRequests` claim
  **per batch with no scan window**, so a function scanning past the
  on-chain view's 25-batch `MAX_BATCH_SCAN` finds work the view
  structurally cannot — and the executor accepts it. That depth is W1's whole
  value.
- `StrategyKeeperExecutor._processReport` re-derives every quantity with the
  **same bounded helpers** its view uses, so a "truer" off-chain number would
  be rejected on arrival. Its own `checker()` is therefore exactly as good as
  any off-chain decider could be — so W2's function only relays its payload
  verbatim, and a modified payload is structurally impossible: the bytes come
  from the contract view, not from function code.

## The W1 tick shape

`mimic-functions/queue-keeper/src/function.ts`:

```mermaid
flowchart TB
    T["trigger fires"] --> P["read pause flags (executor, Controller,<br/>ExitQueue, AMM), cursor, knobs,<br/>tick timestamp (ms → s)"]
    P -- "anything paused" --> NONE1["emit nothing<br/>(every action would revert)"]
    P --> S["full scan cursor → current:<br/>batchInfo + unprocessedUsersCount,<br/>then user lists + requestInfo for candidates"]
    S --> D["decide(state)<br/>(pure, src/decide.ts)"]
    D --> X["cross-check vs queueUpkeepStatus()<br/>(src/divergence.ts)"]
    X -- "class = bug" --> EL["log Error — must stay at zero"]
    X -- "match / intended-improvement /<br/>truncated-scan" --> IL["log Info"]
    EL --> NA{"action == None?"}
    IL --> NA
    NA -- "yes" --> NONE2["emit nothing, with reason logged"]
    NA -- "no" --> E["encode perform params<br/>(src/params.ts — ids and indices only)"]
    E --> OUT["EvmCall intent to the executor<br/>(selector from the generated wrapper)"]
```

One clock, deliberately: `state.now` is the tick timestamp converted to
seconds — batch ages are compared against `createdAt` values recorded from
`block.timestamp`, and a wall clock (or the runner's raw milliseconds) would
skew every `minBatchAge` check by seconds or 1000×.
