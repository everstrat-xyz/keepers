# W4 — freeze-watch

Observability for freeze precursors and keeper health. **No on-chain writes.**

That is structural, not a convention: nothing in this workflow's dependency
graph can build or send a transaction, so there is no code path from an alert
to one. Adding one would mean adding write-capable code, which is visible in
review. NAV guardian *actuation* is a separate epic gated on DAO sign-off
(TECH_SPEC Phase 3).

> W4 is the last CRE-era workflow in this repo — W1/W2 migrated to Mimic. Its
> own migration is deferred; until then its keeper-health check reads the
> vendor-neutral executor surface (`executorCallerCount`, `isExecutorCaller`).

## Alerts

| Kind | Severity | Fires when |
| --- | --- | --- |
| `protocol-paused` | critical | Controller / ExitQueue / AMM / StrategyManager / an executor is paused |
| `oracle-stale` | critical | a feed's price is older than `oracleStaleAfterSeconds` |
| `batch-escape-hatch` | warning → critical | a priced batch nears, then reaches `pricedAt + MAX` (`now ≥` deadline — ~1s before W1/W2's strict `>`) |
| `keeper-stalled` | critical | upkeep was available but no perform has succeeded for `keeperStalledAfterSeconds` |
| `receiver-unbound` | warning | the executor's caller allowlist is empty, or the configured Mimic proxy is missing from it — `perform()` reverts for every task |
| `upkeep-backlog` | warning | `backlogWarnBatches` batches are waiting behind the cursor |
| `strategy-unhealthy` | warning | a live strategy reports unhealthy |
| `strategy-call-failure` | warning | a `Strategy*Failed` event was observed |

Thresholds live in `config.<target>.json`; `freezewatch.DefaultThresholds()`
supplies conservative fallbacks. Tune against `FREEZE_RUNBOOK.md` in the
contracts repo.

## Design choices worth knowing

**Alerting is skipped when nothing fires.** W4 runs every ~10 minutes; a channel
that receives "all clear" 144 times a day is a channel nobody reads. The cost is
that silence is ambiguous between healthy and W4-is-down — left to CRE's own
execution monitoring rather than solved with heartbeat spam.

**A stalled keeper needs available upkeep.** A keeper that has not acted because
there was nothing to do is working correctly. Alerting on idleness alone would
make the signal useless within a day.

**Unread is not stale.** A feed whose read failed has a zero timestamp, not an
infinitely old one, and does not alert — otherwise a transient RPC failure
becomes a page.

**Partial reads are tolerated.** A contract W4 cannot reach produces no alert
for that check rather than a failed tick. A monitor that goes silent exactly
when the protocol misbehaves is worse than one with a blind spot it reports.
`readsRemaining` and the `*Watched` counts are logged every tick so the blind
spot is visible.

## Not covered

- **CRE credit balance.** The issue lists a low-balance alert. CRE meters
  workflow execution in **credits** billed through the CRE UI, not an on-chain
  LINK balance, so there is nothing for a workflow to read. This needs the CRE
  API or UI monitoring and is not implementable here.
- **Pool observation cardinality vs `twapInterval`.** Needs the Uniswap pool
  address, which arrives with `DeployUniCLStrat`.
- **Deposit→withdraw→deposit cycling** and **`WithdrawalCompleted` below
  requested.** Both need historical event analysis across more than CRE's
  100-block `LogQueryBlockLimit`, so they need a windowed design of their own.

## Secrets

`ALERT_WEBHOOK_URL` (see `secrets.yaml`) is the destination. It is a secret
because anyone holding it can post to the ops channel.

Staging ships `dryRun: true` — alerts are evaluated and logged, nothing is
posted — so W4 is safe to deploy before a webhook exists.

## Verifying a change

```bash
make test
cre workflow simulate freeze-watch --target local-settings --non-interactive --trigger-index 0
```

See [`docs/LOCAL_FORK.md`](../docs/LOCAL_FORK.md). Pausing the Controller on the
fork is the quickest way to confirm alerts fire and clear.
