# Gelato cutover runbook

How to move EverStrat's keeper plane from the retired Chainlink CRE workflows
to Gelato tasks, executor-by-executor, with a verification gate at every step.

The order matters: executors deploy **inert** (an empty caller allowlist makes
`perform()` revert `KeeperExecutorNoAllowedCallers`), so it is always safe to
deploy contracts first and bind tasks after.

---

## 0. Preconditions

| What | Where | Check |
| --- | --- | --- |
| Executors deployed | `everstrat-xyz/contracts` `DeployKeeperExecutors` | `executorCallerCount() == 0` on both |
| Gelato account funded | [app.gelato.network](https://app.gelato.network) → 1Balance | enough credit for ~1 task-tx/30min |
| Registry addresses known | deploy output / `registry.getContractByKey` | note `QUEUE_KEEPER_EXECUTOR`, `STRATEGY_KEEPER_EXECUTOR` |
| ADMIN_ROLE signer available | the DAO/multisig that holds it | it must call `allowExecutorCaller` |

The proxy address Gelato will use **does not exist yet** — it is assigned when
the task is created. That is why the allowlist is a settable list rather than
an immutable constructor arg.

---

## 1. W2 — StrategyKeeperExecutor (solidity resolver)

W2 has no task-side code: the contract's own `checker()` is the resolver.

### 1.1 Create the task

Gelato dashboard → **Create Task → Resolver**:

| Field | Value |
| --- | --- |
| Contract address | the deployed `StrategyKeeperExecutor` |
| Function | `checker()` |
| Trigger | every 5 minutes (or per ops policy) |
| Chain | the deployment chain |

### 1.2 Find the dedicated proxy

Task detail page → **Dedicated Msg Sender**. Record it — it is the only
address that will ever call `perform()`.

### 1.3 Bind it (ADMIN_ROLE)

```solidity
StrategyKeeperExecutor.allowExecutorCaller(<gelato-proxy>);
```

Verify:

```
isExecutorCaller(<gelato-proxy>) == true
executorCallerCount() == 1
```

### 1.4 Verify the resolver

`checker()` must return `(canExec, execPayload)`:

- no work due → `canExec == false` with a reason string
- work due → `canExec == true` and `execPayload` decodes to
  `perform(uint8 action)` for the top-priority action

Gelato's task page shows each resolver poll and its result — watch one full
poll cycle before declaring W2 live.

---

## 2. W1 — QueueKeeperExecutor (TypeScript Web3 Function)

### 2.1 Configure

`web3-functions/queue-keeper` user args:

```json
{
  "registryAddress": "0x…",
  "queueExecutorAddress": "0x…"
}
```

Optional: `maxBatchScan` (default 250) caps the off-chain scan width per tick.
Protocol addresses (controller/exitQueue/amm) are **not** configured — they are
resolved from the Registry at every tick.

### 2.2 Test locally first

```bash
cd web3-functions/queue-keeper
npm install --ignore-scripts
npm run typecheck
npm test
```

Then a dry-run against staging if available (Gelato's dashboard has a
simulator that runs `onRun` without submitting).

### 2.3 Create the task

Gelato dashboard → **Create Task → Web3 Function**, pointing at the
`queue-keeper` function, with a time-based trigger (every 1–5 minutes).

### 2.4 Find the dedicated proxy

As with W2 — task detail → **Dedicated Msg Sender**.

### 2.5 Bind it (ADMIN_ROLE)

```solidity
QueueKeeperExecutor.allowExecutorCaller(<gelato-proxy>);
```

Verify: `isExecutorCaller(<gelato-proxy>) == true`.

### 2.6 Verify a full tick

The W3F logs `W1 queue-keeper: action=… batch=… end=… divergence=…` every run:

| Divergence | Meaning | Action |
| --- | --- | --- |
| `match` | run agrees with the on-chain view | none |
| `intended-improvement` | run found work beyond the view's scan window | none — this is the point of W1 |
| `truncated-scan` | the scan cap stopped the walk short | none, unless it persists across ticks |
| `bug` | unexplained disagreement | **stop and investigate** |

A `bug` divergence means either the off-chain model or the read layer is
wrong. Do not leave a W1 in that state: pause the Gelato task while
investigating.

---

## 3. Shadow-mode graduation (optional but recommended)

Before trusting W1 with live settlement, run it in Gelato's dry-run/simulate
mode for a window (the CRE-era rule of thumb was 7 days with **zero
unexplained divergences**). The divergence classes above are exactly what
"explained" means; anything outside them blocks graduation.

---

## 4. W4 — freeze-watch

Out of scope here (deferred). When it migrates, its keeper-health check no
longer reads CRE binding views — it reads `executorCallerCount()` and, when
`gelatoProxyAddress` is configured, `isExecutorCaller(proxy)`. An empty
allowlist or a missing proxy is reported as bound-but-broken, which is the
state W4 exists to catch.

---

## 5. Rollback

| Situation | Action |
| --- | --- |
| Task misbehaving | Pause the task in the Gelato dashboard — `perform()` stops being called; nothing on-chain to revert |
| Executor misbehaving | `pause()` on the executor (ADMIN or SECURITY role) — every action path reverts `EnforcedPause` |
| Rotate the Gelato proxy | `removeExecutorCaller(old)`, `allowExecutorCaller(new)` — redeploying the task creates a new proxy |

The executors never hold funds (W1 credits land in `amm.claimableBalances`,
pull-over-push), so pausing loses nothing but time.

---

## 6. Failure modes and their error selectors

| Selector | Thrown when | Seen by |
| --- | --- | --- |
| `KeeperExecutorNoAllowedCallers` | allowlist is empty — executor still inert | first `perform()` after deploy, before step 1.3/2.5 |
| `KeeperExecutorUnauthorizedCaller` | proxy not allowlisted | a rotated/recreated task that was never re-bound |
| `KeeperExecutorNoUpkeepNeeded` | claim re-validated against live state and rejected | a stale payload, or two tasks racing |
| `EnforcedPause` | executor paused | deliberate ops pause |

A stream of `KeeperExecutorNoUpkeepNeeded` on Gelato's task page usually means
two tasks point at the same executor — one wins the race, the other's claim
goes stale. There should be exactly one task per executor.
