# Mimic cutover runbook

How to move EverStrat's keeper plane from the retired Chainlink CRE workflows
(and the briefly-attempted Gelato tasks) to the
[Mimic Network](https://mimic.fi), executor-by-executor, with a verification
gate at every step.

The order matters: executors deploy **inert** (an empty caller allowlist makes
`perform()` revert `KeeperExecutorNoAllowedCallers`), so it is always safe to
deploy contracts first and bind tasks after.

---

## 0. Preconditions

| What | Where | Check |
| --- | --- | --- |
| Executors deployed | `everstrat-xyz/contracts` `DeployKeeperExecutors` | `executorCallerCount() == 0` on both |
| Mimic account funded | [Mimic Protocol App](https://mimic.fi) | enough credit for ~1 task-tx/30min |
| Registry addresses known | deploy output / `registry.getContractByKey` | note `QUEUE_KEEPER_EXECUTOR`, `STRATEGY_KEEPER_EXECUTOR` |
| ADMIN_ROLE signer available | the DAO/multisig that holds it | it must call `allowExecutorCaller` |

The smart-account address Mimic will use **does not exist yet** — it is
assigned when the task is created. That is why the allowlist is a settable
list rather than an immutable constructor arg.

---

## 1. W2 — StrategyKeeperExecutor (checker relay)

W2's function is a pure relay: the contract's own `checker()` decides, and
`mimic-functions/strategy-keeper` forwards its `execPayload` verbatim.

### 1.1 Deploy the function

```bash
cd mimic-functions/strategy-keeper
npm install
npm test          # compile + mocha through the raw-mock harness
mimic deploy
```

### 1.2 Create the task

Mimic Protocol App → create a task from the deployed `strategy-keeper`
function, time-based trigger (every 5 minutes, or per ops policy), on the
deployment chain. Configure the function inputs:

```json
{
  "chainId": 10,
  "executor": "0x…",
  "smartAccount": "<assigned below, then update>",
  "maxFee": "1"
}
```

`smartAccount` is the Mimic signer the executor must allowlist — set it once
the task exists (next step).

### 1.3 Find the dedicated signer

Task detail → the smart account / dedicated signer address. Record it — it is
the only address that will ever call `perform()`.

### 1.4 Bind it (ADMIN_ROLE)

```solidity
StrategyKeeperExecutor.allowExecutorCaller(<mimic-signer>);
```

Verify:

```
isExecutorCaller(<mimic-signer>) == true
executorCallerCount() == 1
```

### 1.5 Verify the relay

`checker()` must return `(canExec, execPayload)`:

- no work due → `canExec == false`, function logs and emits nothing
- work due → `canExec == true` and the intent's calldata equals `execPayload`
  byte-for-byte

Watch one full poll cycle before declaring W2 live.

---

## 2. W1 — QueueKeeperExecutor (deep-scan function)

### 2.1 Configure

`mimic-functions/queue-keeper` function inputs:

```json
{
  "chainId": 10,
  "executor": "0x…",
  "controller": "0x…",
  "exitQueue": "0x…",
  "amm": "0x…",
  "smartAccount": "0x…",
  "maxBatches": 250,
  "maxRequestsPerBatch": 50,
  "maxFee": "1"
}
```

Every key above is required by `manifest.yaml`; a missing one fails manifest
validation when the trigger is created. `scripts/create-trigger.ts` builds the
same set from `scripts/.env` (see `scripts/env.template`).

`maxBatches` caps the off-chain scan width per tick. Protocol addresses are
passed in rather than resolved from the Registry — W1 reads them before
anything else, and a per-tick Registry round-trip buys nothing while the
address book is timelocked. The **AMM** address is needed only for its pause
flag: `_queueUpkeepStatus` refuses to recommend work while the AMM is paused,
and W1 has to refuse for the same reason (`Controller.priceBatch` is
`whenNotPaused` on the Controller alone, so an AMM-only pause would not stop
the transaction).

### 2.2 Test locally first

```bash
cd mimic-functions/queue-keeper
npm install
npm test
```

`tests/function.spec.ts` runs the compiled WASM through a raw-mock oracle
harness: nine scenarios (the four pause paths, price/process/advance,
truncated-scan, no-work) plus a `divergence cross-check` block asserting the
`match` / `intended-improvement` / `bug` classification against a mocked
`queueUpkeepStatus`.

### 2.3 Deploy the function and create the task

```bash
cd mimic-functions/queue-keeper
mimic deploy
```

Then create the task in the Protocol App with a time-based trigger
(every 1–5 minutes) and the inputs above, with `smartAccount` filled from the
task's assigned signer.

### 2.4 Bind it (ADMIN_ROLE)

```solidity
QueueKeeperExecutor.allowExecutorCaller(<mimic-signer>);
```

Verify: `isExecutorCaller(<mimic-signer>) == true`.

### 2.5 Verify a full tick

The function logs `W1 queue-keeper: action=… batch=… end=… divergence=…`
every run:

| Divergence | Meaning | Action |
| --- | --- | --- |
| `match` | run agrees with the on-chain view | none |
| `intended-improvement` | run found work beyond the view's scan window | none — this is the point of W1 |
| `truncated-scan` | the scan cap stopped the walk short | none, unless it persists across ticks |
| `bug` | unexplained disagreement | **stop and investigate** |

A `bug` divergence means either the off-chain model or the read layer is
wrong. Do not leave a W1 in that state: pause the Mimic task while
investigating.

---

## 3. Shadow-mode graduation (optional but recommended)

Before trusting W1 with live settlement, run it without allowlisting the
signer (or with the task in simulate mode) for a window — the CRE-era rule of
thumb was 7 days with **zero unexplained divergences**. The divergence classes
above are exactly what "explained" means; anything outside them blocks
graduation.

---

## 4. W4 — freeze-watch (removed)

W4 was the read-only freeze-precursor and keeper-health watcher, and it has
been removed rather than carried along unmigrated (see the README). Its
keeper-health check is the part worth rebuilding wherever monitoring lands:
`executorCallerCount() == 0` means the executor is inert, and a configured
smart account that fails `isExecutorCaller()` means bound-but-broken — a
keeper that will never fire and reverts if it tries.

Until something watches that, step 1.4 / 2.4's verification is the only thing
confirming the binding, and nothing re-checks it afterwards. A rotated trigger
that was never re-bound is silent.

---

## 5. Rollback

| Situation | Action |
| --- | --- |
| Task misbehaving | Pause the task in the Mimic app — `perform()` stops being called; nothing on-chain to revert |
| Executor misbehaving | `pause()` on the executor (ADMIN or SECURITY role) — every action path reverts `EnforcedPause` |
| Rotate the Mimic signer | `removeExecutorCaller(old)`, `allowExecutorCaller(new)` — recreating the task assigns a new signer |

The executors never hold funds (W1 credits land in `amm.claimableBalances`,
pull-over-push), so pausing loses nothing but time.

---

## 6. Failure modes and their error selectors

| Selector | Thrown when | Seen by |
| --- | --- | --- |
| `KeeperExecutorNoAllowedCallers` | allowlist is empty — executor still inert | first `perform()` after deploy, before step 1.4/2.4 |
| `KeeperExecutorUnauthorizedCaller` | signer not allowlisted | a rotated/recreated task that was never re-bound |
| `KeeperExecutorNoUpkeepNeeded` | claim re-validated against live state and rejected | a stale payload, or two tasks racing |
| `EnforcedPause` | executor paused | deliberate ops pause |

A stream of `KeeperExecutorNoUpkeepNeeded` on the task page usually means two
tasks point at the same executor — one wins the race, the other's claim goes
stale. There should be exactly one task per executor.
