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

W2's function is a relay: the contract's own `checker()` decides, and
`mimic-functions/strategy-keeper` forwards its `execPayload` verbatim. What it
does choose is the intent's max fee, per action (§1.5).

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
  "maxFee": "50",
  "rebalanceMaxGwei": "0.4",
  "syncMaxGwei": "0.25",
  "withdrawMaxGwei": "0.3",
  "withdrawUrgentMaxGwei": "3",
  "withdrawRampStartHours": 24,
  "withdrawRampEndHours": 12,
  "amountFeeBps": 115
}
```

Every key is required by `manifest.yaml`; `scripts/inputs.ts` fills the fee
keys with these defaults unless `scripts/.env` overrides them. A malformed or
inconsistent fee input makes every tick log `W2 config-error` naming the field
and emit nothing — check the first execution's logs after creating the task.

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

Watch one full poll cycle before declaring W2 live. The relay log line names
the action and how its max fee was built.

### 1.5 Fee caps

A solver quotes ≈ gasUsed × gas price × its markup, and cannot fill above the
intent's max fee. So the max fee is a gas-price ceiling: during a spike the
intent lapses and the next 5-minute tick retries, at the cost of an idle tick.
W2 sets it per action, by what waiting costs, converts it to USD at the Mimic
oracle's native-token price, and clamps it to `maxFee`.

| action | what waiting costs | cap |
|---|---|---|
| WithdrawShortfall | A priced exit is committed and strands at `pricedAt + 3 days` | `withdrawMaxGwei` until `withdrawRampStartHours` before the earliest in-window batch with unprocessed users expires, linear to `withdrawUrgentMaxGwei` at `withdrawRampEndHours`. No amount term: `minWithdrawETH` can be 1e14 |
| Rebalance | ~$0.06/h of LP fees for the largest strategy | `rebalanceMaxGwei`, raised to the withdrawal ramp when a batch nears expiry — `checker()` hides the WithdrawShortfall behind a pending Rebalance |
| Sync | NAV lags by at most a day of unpoked fees (~0.08% of NAV) | `syncMaxGwei` |
| DepositExcess, HarvestPerformanceFees, ProvideExitLiquidity | Yield on idle ETH only | `amountFeeBps` of `strategyUpkeepStatus().amount` |
| undecodable payload | — | flat `maxFee` |

A gwei ceiling becomes a fee through a gas budget per strategy the action
touches — Rebalance 1.69M per selected strategy, Sync 210k and
WithdrawShortfall 980k per registered strategy — times a 1.45 solver markup.
Adding a strategy scales the budget; no input needs to change.

**Where the defaults come from** (analysis of 2026-09-24):

- *Solver pricing* — W2's 13 mainnet settlements 2026-09-20..23, priced with
  Chainlink ETH/USD at each block. Fee over gas cost: 1.03–1.41 (one 0.74 where
  the base fee rose after the quote); worst per action sets the 1.45 markup.
  Gas: Rebalance 1.45–1.69M (one strategy each), Sync 0.52–0.60M,
  DepositExcess 2.87–3.14M, WithdrawShortfall 2.94M (three strategies).
  Solver tip 0.01–0.39 gwei, median ~0.15.
- *Gas market* — 30 days of mainnet base fees (blocks 25,827,075–26,043,075).
  Median 0.073 gwei, p90 0.33, p99 1.40, max 6.3. At the ceiling (base + 0.15
  tip), share of 5-minute ticks that fill / longest stretch blocked:

  | ceiling | fills | longest blocked |
  |---|---|---|
  | 0.25 gwei | 62% | 18.2h |
  | 0.30 gwei | 75% | 15.5h |
  | 0.40 gwei | 86% | 11.8h |
  | 1.00 gwei | 97% | 2.3h |
  | 3.00 gwei | 99.8% | 0.3h |

  The withdrawal floor was never blocked longer than 15.5h against a 72h
  window; the ramp is for the tail beyond that sample.
- *Protocol* — LP fees collected by the three strategies since 2026-09-12:
  $9.26, ~$2.19 per ETH-day (~30% APR, lower bound: uncollected fees excluded).
  One rebalance per strategy in that time (~8 days in range). Every exit batch
  so far was processed 5–35 minutes after pricing. `performanceFeeBps` is 0 on
  mainnet, so HarvestPerformanceFees does not fire today.
- *Deposit share* — 115 bps pays back in ~14 days at $2.19 per ETH-day. At the
  0.1 ETH `minDepositETH` that is ~$3.1, a ~0.3 gwei ceiling; larger deposits
  clear higher gas.

Replaying the 13 settlements under these caps: 8 fill unchanged, 5 defer
2.6–9.9h, and total solver spend drops from $34.20 to ~$21.

Revisit the ceilings, not the ETH price: caps follow the oracle price on every
tick. Revisit the gas budgets if a strategy type with a different gas profile
is added, and `maxFee` if ETH moves enough that the urgent withdrawal cap
(~$35 at ETH $2,700) approaches it.

**Replacing a live trigger.** The inputs are new, so an existing W2 trigger
cannot be edited into this version: deploy the function, create a new trigger
with the same `smartAccount` (the executor allowlist is unchanged), confirm its
first ticks log `W2 strategy-keeper: no upkeep` or `W2 relay`, then disable the
old trigger. Two live W2 triggers race each other for the same work.

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
  "helper": "0x…",
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

The **helper** address is Mimic's own `MimicHelper`, used for the single
Controller-balance read. It is an input for the same reason the others are:
`environment.getNativeTokenBalance` in lib-ts pins one helper address for
every chain, and a chain whose helper lives elsewhere answers `0x` — which
surfaces as an ABI decode overrun that aborts the whole tick, not as a zero
balance. Look the helper up per chain rather than trusting the lib-ts
default. On Base Sepolia it is
`0x5cf82cBED1110fc2f75B3413d53abac492931804`.

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
