# Mimic cutover runbook

How to move EverStrat's keeper plane from the retired Chainlink CRE workflows
(and the briefly-attempted Gelato tasks) to the
[Mimic Network](https://mimic.fi), executor-by-executor, with a verification
gate at every step.

The order matters: executors deploy **inert** (an empty caller allowlist makes
`perform()` revert `KeeperExecutorNoAllowedCallers`), so it is always safe to
deploy contracts first and bind tasks after.

---

## Smart account wiring

Three addresses, not one:

| Address | Role |
| --- | --- |
| Your Mimic EOA (`PRIVATE_KEY`) | Signs trigger creation off-chain. Never calls `perform()`. |
| Mimic smart account | `msg.sender` of `perform()`. Function input `smartAccount` (WASM `.addUser`) **and** `allowExecutorCaller` — the same value. |
| Executor contract | Holds `KEEPER_ROLE`. Allowlists the smart account. |

`mimic deploy` only publishes WASM (a `FUNCTION_CID`). It does not read
`scripts/.env`. That file is local: `try-function`, `prefill-url`, and
`create-trigger` use it. The Protocol App never loads it. `prefill-url`
copies current `.env` values into a form URL (one-way).

**Live path:** look up the smart account for this chain in the Mimic Protocol
App *before* signing a trigger. Put it in `.env` as `SMART_ACCOUNT_ADDRESS`,
in the trigger input, and in `allowExecutorCaller`. Confirm the task page
shows that same address.

**Dry-run / prefill:** `try-function` and `prefill-url` may use `0x0` because
they do not settle. Do **not** submit a live trigger with `0x0` — the WASM
would `.addUser` the zero address, and allowlisting a different SA would not
help. Paste the real account into the App form before signing.

The allowlist is settable because that account is not known when the executor
is deployed, and recreating a task can rotate it: update the trigger input
(sign a new version) and `removeExecutorCaller(old)` / `allowExecutorCaller(new)`.

---

## 0. Preconditions

| What | Where | Check |
| --- | --- | --- |
| Executors deployed | `everstrat-xyz/contracts` `DeployKeeperExecutors` | `executorCallerCount() == 0` on both |
| Mimic account funded | [Mimic Protocol App](https://mimic.fi) | enough credit for ~1 task-tx/30min |
| Registry addresses known | deploy output / `registry.getContractByKey` | note `QUEUE_KEEPER_EXECUTOR`, `STRATEGY_KEEPER_EXECUTOR` |
| ADMIN_ROLE signer available | the DAO/multisig that holds it | it must call `allowExecutorCaller` |
| Mimic smart account (this chain) | Protocol App | same address that will go in the trigger input and `allowExecutorCaller` |

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
  "smartAccount": "0x…",
  "maxFee": "1"
}
```

`smartAccount` is the Mimic account for this chain (see **Smart account
wiring**). Look it up in the App *before* signing. It is also the address
ADMIN passes to `allowExecutorCaller`. After create, confirm the task page
shows that same address.

### 1.3 Bind it (ADMIN_ROLE)

```solidity
StrategyKeeperExecutor.allowExecutorCaller(<mimic-signer>);
```

Verify:

```
isExecutorCaller(<mimic-signer>) == true
executorCallerCount() == 1
```

### 1.4 Verify the relay

`checker()` must return `(canExec, execPayload)`:

- no work due → `canExec == false`, function logs and emits nothing
- work due → `canExec == true` and the intent's calldata equals `execPayload`
  byte-for-byte

Watch one full poll cycle before declaring W2 live.

---

## 2. W1 — QueueKeeperExecutor

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

`maxBatches` (default 250) caps the off-chain header walk per tick so a
tick cannot be unbounded. It is not a second live-priced cap:
`MAX_BATCH_SCAN` already equals `MAX_LIVE_PRICED_BATCHES` (25), and
`priceBatch` will not create a 26th. Truncation (`scanTruncatedAt`) needs
`current - cursor ≥ 250`, or a tiny cap — the spec uses `maxBatches: 2`.
Do not treat it as a production cadence event. With W1 as the only
performer, a live batch the view cannot see also does not show up (every
`perform` peeks +25 skippable; a down W1 does not price). See the README
"Why the split".

Protocol addresses are passed in rather than resolved from the Registry — W1
reads them before anything else, and a per-tick Registry round-trip buys
nothing while the address book is timelocked. The **AMM** address is needed
only for its pause flag: `_queueUpkeepStatus` refuses to recommend work while
the AMM is paused, and W1 has to refuse for the same reason
(`Controller.priceBatch` is `whenNotPaused` on the Controller alone, so an
AMM-only pause would not stop the transaction).

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

### 2.3 Then dry-run it against the real deployment

The specs mock the oracle, keyed by the same query hash the function computes —
so they cannot catch a query the oracle rejects or a return the generated
wrapper decodes wrongly. `try-function` closes that gap: same compiled WASM,
live oracle, real addresses, and it settles nothing.

```bash
cp scripts/env.template scripts/.env   # fill in the deployed addresses
npm run build
npm run try-function
```

It prints the decision line and the cross-check verdict. **Do not create a
trigger while that says `divergence=bug`** — the off-chain model disagrees with
`queueUpkeepStatus()` in a way the scan window does not explain, and binding a
signer then just makes it expensive.

This is also the honest way to run shadow mode. Creating a live trigger without
allowlisting the signer does *not* observe quietly: every tick submits an intent
that reverts `KeeperExecutorUnauthorizedCaller`, which costs fees and looks
exactly like a broken keeper.

### 2.4 Deploy the function and create the task

```bash
cd mimic-functions/queue-keeper
mimic deploy
```

Then create the task in the Protocol App with a time-based trigger
(every 1–5 minutes) and the inputs above, with `smartAccount` already filled
from the App (same address you will allowlist). Do not create with `0x0`.

### 2.5 Bind it (ADMIN_ROLE)

```solidity
QueueKeeperExecutor.allowExecutorCaller(<mimic-signer>);
```

Verify: `isExecutorCaller(<mimic-signer>) == true`.

### 2.6 Verify a full tick

The function logs `W1 queue-keeper: action=… batch=… end=… divergence=…`
every run:

| Divergence | Meaning | Action |
| --- | --- | --- |
| `match` | run agrees with the on-chain view | none |
| `intended-improvement` | shorter prefix than a *mocked* larger view `count`, or a batch past ~`cursor+50` (needs another pricer while this cursor is frozen). Not the default W1-only path | none |
| `truncated-scan` | `maxBatches` stopped the walk short of `current`. Default 250; the spec uses 2 | none, unless it persists across ticks |
| `bug` | unexplained disagreement | **stop and investigate** |

A `bug` divergence means either the off-chain model or the read layer is
wrong. Do not leave a W1 in that state: pause the Mimic task while
investigating.

---

## 3. Shadow-mode graduation (optional but recommended)

Before trusting W1 with live settlement, run `npm run try-function` (§2.3) on a
schedule for a window — the CRE-era rule of thumb was 7 days with **zero
unexplained divergences**. The divergence classes above are exactly what
"explained" means; anything outside them blocks graduation.

Use the dry run, not an unbound live trigger: an unallowlisted trigger reverts
`KeeperExecutorUnauthorizedCaller` every tick, which costs fees and is
indistinguishable from a broken keeper. `try-function` reads the same state
through the same compiled WASM and settles nothing.

Note what shadow mode still will not tell you, because no intent is ever
settled: fee behaviour under `maxFee`, and how often a claim goes stale between
decide and settle (which surfaces as `KeeperExecutorNoUpkeepNeeded`). Both need
a live trigger to measure, and both should be watched in the first days after
binding.

---

## 4. W4 — freeze-watch (removed)

W4 was the read-only freeze-precursor and keeper-health watcher, and it has
been removed rather than carried along unmigrated (see the README). Its
keeper-health check is the part worth rebuilding wherever monitoring lands:
`executorCallerCount() == 0` means the executor is inert, and a configured
smart account that fails `isExecutorCaller()` means bound-but-broken — a
keeper that will never fire and reverts if it tries.

Until something watches that, step 1.3 / 2.5's verification is the only thing
confirming the binding, and nothing re-checks it afterwards. A rotated trigger
that was never re-bound is silent.

---

## 5. Rollback

| Situation | Action |
| --- | --- |
| Task misbehaving | Pause the task in the Mimic app — `perform()` stops being called; nothing on-chain to revert |
| Executor misbehaving | `pause()` on the executor (ADMIN or SECURITY role) — every action path reverts `EnforcedPause` |
| Rotate the Mimic signer | `removeExecutorCaller(old)`, `allowExecutorCaller(new)` — recreating the task assigns a new signer |
| Trigger expired | Mimic requires an `endDate` on every trigger (`scripts/inputs.ts`, `TRIGGER_END_DATE`). When it passes the keeper simply stops, with **no on-chain signal** — recreate the trigger and re-bind its new signer |

The executors never hold funds (W1 credits land in `amm.claimableBalances`,
pull-over-push), so pausing loses nothing but time.

---

## 6. Failure modes and their error selectors

| Selector | Thrown when | Seen by |
| --- | --- | --- |
| `KeeperExecutorNoAllowedCallers` | allowlist is empty — executor still inert | first `perform()` after deploy, before step 1.3/2.5 |
| `KeeperExecutorUnauthorizedCaller` | signer not allowlisted | a rotated/recreated task that was never re-bound |
| `KeeperExecutorNoUpkeepNeeded` | claim re-validated against live state and rejected | a stale payload, or two tasks racing |
| `EnforcedPause` | executor paused | deliberate ops pause |

A stream of `KeeperExecutorNoUpkeepNeeded` on the task page usually means two
tasks point at the same executor — one wins the race, the other's claim goes
stale. There should be exactly one task per executor.
