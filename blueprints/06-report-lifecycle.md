# 06 — Report lifecycle: envelope → DON signature → receiver

Files: `pkg/envelope/envelope.go`, `pkg/crewrite/crewrite.go`,
`pkg/queue/queue.go` (params encoding), `pkg/strategy/strategy.go` (Report.Build),
and the receivers' `CREReceiverBase.onReport`.

## The envelope

The wire format is exactly `abi.encode(Envelope)` — one dynamic tuple:

```mermaid
flowchart LR
    subgraph ENV["Envelope (ICREReceiverBase.Envelope)"]
        F1["chainSelector: uint64<br/>must equal receiver's immutable"]
        F2["sequence: uint64<br/>must be > lastSequence,<br/>read fresh every tick"]
        F3["observedAt: uint64<br/>the OBSERVED BLOCK's timestamp,<br/>never runtime.Now()"]
        F4["action: uint8<br/>receiver-specific enum"]
        F5["params: bytes<br/>ABI-encoded hints —<br/>NEVER an amount"]
    end
```

**The hard constraint:** `params` carries claims and hints only. No ETH
amount, NAV, or price can appear — the builders make it inexpressible:

- `pkg/queue` params take only batch ids and an end index, all `uint64`, and
  `DecodeParams` enforces the **exact** wire length per action (a smuggled
  amount could only appear as a trailing word that Solidity's `abi.decode`
  would silently ignore).
- `pkg/strategy`'s `Report.Build` takes an action and *nothing else*.

## Pre-flight validation — `envelope.Validate`

Applied against the receiver's **live** state before emitting, so a report
the contract would reject never leaves the workflow:

```mermaid
flowchart TB
    V["Validate(receiverState, now)"] --> G1{"chainSelector ==<br/>receiver.CHAIN_SELECTOR?"}
    G1 -- "no" --> E1["ErrWrongChain"]
    G1 -- "yes" --> G2{"sequence &gt;<br/>receiver.lastSequence?"}
    G2 -- "no" --> E2["ErrReplayedSequence"]
    G2 -- "yes" --> G3{"observedAt &lt;= now?<br/>(now = delivery-time estimate,<br/>runtime.Now() is the ONLY<br/>legitimate wall-clock use)"}
    G3 -- "no" --> E3["ErrObservedInFuture<br/>the receiver applies this with<br/>ZERO tolerance vs block.timestamp"]
    G3 -- "yes" --> G4{"now - observedAt &lt;=<br/>MAX_REPORT_AGE?"}
    G4 -- "no" --> E4["ErrStaleReport"]
    G4 -- "yes" --> OK["emit"]
    style E3 fill:#ffebee
    style E4 fill:#fff8e1
```

## Sequence — `NextSequence`

`sequence` is **not** workflow-owned state. A break-glass multisig report, a
receiver redeploy, or overlapping workflow versions all move `lastSequence`.
So every tick reads `lastSequence()` from the receiver and proposes
`lastSequence + 1` — a local counter would leave the keeper permanently
behind, with every report rejected as a replay.

## Delivery — `crewrite.Write`

```mermaid
sequenceDiagram
    participant WF as W1/W2 handler
    participant RT as runtime (DON)
    participant EV as evm.Client
    participant FWD as KeystoneForwarder
    participant RX as Receiver (onReport)

    WF->>RT: GenerateReport(payload, ecdsa/keccak256)
    RT-->>WF: DON report (F+1 signatures + workflow identity)
    WF->>EV: WriteReport(receiver, report, gasLimit=2M default)
    EV->>FWD: forwarder.verifyAndForward()
    FWD->>RX: onReport(report)
    RX->>RX: envelope guards (above)
    RX->>RX: re-derive every quantity from live state
    alt claims hold
        RX-->>FWD: execute (KEEPER_ROLE action)
        FWD-->>EV: success
    else claims stale / wrong
        RX-->>FWD: revert KeeperExecutorNoUpkeepNeeded
        FWD-->>EV: tx landed, receiver reverted
    end
    EV-->>WF: Result{TxStatus, ReceiverReverted, TxHash}
```

Three outcomes the workflows distinguish (`crewrite.Result`):

| Outcome | Treated as |
| --- | --- |
| tx landed + receiver accepted | success — `Result.Wrote = true` |
| tx landed + receiver reverted (`KeeperExecutorNoUpkeepNeeded`) | successful *tick* with a warning — state moved between observation and delivery; alert on the *rate*, not the occurrence (W4, issue #7) |
| tx failed | workflow error |

## Why the whole design hangs together

```mermaid
flowchart LR
    BUG["workflow bug"] -->|report carries only claims| RX2["receiver re-derives everything<br/>→ worst case a revert,<br/>never a wrong settlement"]
    AMT["smuggled amount" ] -->|params length check +<br/>amount-inexpressible builders| X["cannot be expressed"]
    CLOCK["DON clock ahead of chain"] -->|observedAt from<br/>evmread.BlockTimestamp| OK2["reports keep landing"]
    SEQ["external report moves lastSequence"] -->|sequence read fresh<br/>every tick| OK3["keeper never wedges"]
    STALL["long stall grows the queue"] -->|budgeted scan truncates| OK4["tick under-proposes,<br/>W4 flags the backlog"]
```
