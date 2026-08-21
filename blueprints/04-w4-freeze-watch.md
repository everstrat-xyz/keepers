# 04 — W4 freeze-watch (observability, no writes)

Files: `freeze-watch/main.go`, `freeze-watch/reads.go`, `pkg/freezewatch/alerts.go`.

W4 is the silent-stop detector: it watches freeze precursors and keeper health
and posts an alert digest to a webhook. It **cannot write on-chain** — the
package imports neither `pkg/crewrite` nor `pkg/envelope`, so actuation would
require adding an import that is visible in review. NAV-guardian actuation is a
separate epic behind DAO sign-off (TECH_SPEC Phase 3).

## Tick flow

```mermaid
flowchart TB
    T["cron fires"] --> RES["chains.ByName + parse registry address"]
    RES -- "placeholder config" --> Q["quiet no-op: Result{bound: false}<br/>— a failing monitor would page<br/>someone about itself"]
    RES -- "ok" --> RD["readObservation — budget 15 reads:<br/>protocol pause flags, batch states,<br/>oracle feed freshness, strategy health,<br/>failure events, receiver liveness"]

    RD --> EV["freezewatch.Evaluate(obs, thresholds)<br/>— pure, never returns an error.<br/>A monitor that fails to evaluate goes<br/>quiet exactly when things go wrong."]
    EV --> LOG["log every alert<br/>(critical → Error, warning → Warn)"]
    LOG --> N{"ShouldNotify?<br/>(any alert firing)"}
    N -- "no" --> DONE["Result{all clear}"]
    N -- "yes" --> DRY{"dryRun?"}
    DRY -- "yes" --> D["log only, no webhook"]
    DRY -- "no" --> POST["notify(): fetch ALERT_WEBHOOK_URL secret,<br/>POST JSON digest via HTTP capability,<br/>consensus-agreed (one node cannot<br/>fabricate a delivery)"]
    POST -- "webhook failed" --> WF["log Error, tick still SUCCEEDS —<br/>alerts are already in the logs"]
    POST -- "2xx" --> OK["Result{notified: true}"]

    style Q fill:#e8f0fe
    style WF fill:#fff8e1
```

## Alert kinds — `freezewatch.Evaluate`

```mermaid
flowchart LR
    subgraph CRIT["Severity: critical"]
        C1["protocol-paused<br/>Controller / ExitQueue / AMM /<br/>StrategyManager / receiver paused"]
        C2["oracle-stale<br/>feed older than 3h default —<br/>the thing that tips NAV into freeze"]
        C3["batch-escape-hatch<br/>now ≥ pricedAt + MAX<br/>(1s earlier than W1/W2's strict &gt;)<br/>with unprocessed requests"]
        C4["keeper-stalled<br/>upkeep available but no report<br/>accepted for &gt; 6h default"]
    end
    subgraph WARN["Severity: warning"]
        W1k["batch-escape-hatch<br/>within 12h of the hatch"]
        W2k["upkeep-backlog<br/>≥ 10 batches behind cursor"]
        W3k["receiver-unbound<br/>no workflowId/author set —<br/>receiver rejects everything"]
        W4k["strategy-unhealthy<br/>unhealthy and not paused"]
    end
```

## Why each critical kind exists

| Kind | Failure mode it precedes |
| --- | --- |
| `protocol-paused` | Not a precursor — the protocol is already halted; every keeper action reverts |
| `oracle-stale` | NAV reads revert once the contract's own staleness bound is crossed; W4 fires *before* that (3h default) |
| `batch-escape-hatch` | After the hatch users must close their own requests; the keeper can no longer settle the batch. W4 fires at `now ≥ pricedAt + MAX` so ops see it ~1s before W1/W2/`_batchSettlementCost` (strict `>`) drop the batch from keeper work |
| `keeper-stalled` | The silent-stop mode: work exists, the receiver's own view says so, yet nothing arrives — the thing this workflow exists to catch |

## Liveness check detail — `KeeperHealth`

> **Contract-blocked:** `LastAcceptedAt` has no on-chain source — the receivers
> expose no `lastReportAcceptedAt()` view, so `readKeepers` cannot populate it
> and `keeper-stalled` never fires in production today. The unit tests set the
> field directly, which masked the gap. Unblocking requires a contract accessor
> in `everstrat-xyz/contracts`; see the CONTRACT-BLOCKED note on
> `pkg/freezewatch.KeeperHealth.LastAcceptedAt`.

A keeper is only considered stalled when **all three** hold:

1. `Bound` — the receiver has `expectedWorkflowId`/`expectedAuthor` set (an
   unbound receiver gets the `receiver-unbound` warning instead),
2. `UpkeepAvailable` — the receiver's own view currently recommends an action
   (no work → no stall),
3. `LastAcceptedAt` is older than `KeeperStalledAfter` (default 6h).

## Thresholds

Defaults (`DefaultThresholds`), overridable per-target in
`config.<target>.json`:

| Threshold | Default | Meaning |
| --- | --- | --- |
| `OracleStaleAfter` | 3h | feed age before warning — sits below the contracts' own staleness bound so the alert precedes the revert |
| `EscapeHatchWarnWithin` | 12h | warning window before `MAX_BATCH_PROCESSING_TIME` |
| `KeeperStalledAfter` | 6h | idle-with-work before `keeper-stalled` |
| `BacklogWarnBatches` | 10 | batches waiting behind the cursor |

Tuning guidance lives in `FREEZE_RUNBOOK.md`.
