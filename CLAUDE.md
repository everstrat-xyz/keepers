# CLAUDE.md — EverStrat Keepers

Guidance for Claude Code working in this repo, and the reference the automated
PR reviewer reads. Everything here is a rule that has already cost something to
learn; the "why" matters more than the rule.

## What this repo is

Automation for EverStrat's keeper plane, running on the
[Mimic Network](https://mimic.fi):

- **W1** (`mimic-functions/queue-keeper/`) — a Mimic **Solidity Function**
  (AssemblyScript compiled to WASM) driving `QueueKeeperExecutor`. It
  decides off-chain and submits `perform()` calldata through an
  oracle-signed EvmCall intent. `_execute(ProcessRequests)` has no scan
  window; with W1 as the only performer that extra depth does not show up
  (see §3).
- **W2** (`mimic-functions/strategy-keeper/`) — a thin Mimic relay function.
  `StrategyKeeperExecutor` exposes its own `checker()` returning
  `(canExec, execPayload)`; the function forwards the payload **verbatim** —
  no off-chain re-derivation, no payload interpretation.
W4 (`freeze-watch/`), the read-only freeze-precursor watcher, was removed
along with the Go toolchain and CRE CLI it was the last consumer of. It is in
git history if it comes back.

Governing principle: **functions orchestrate, contracts decide.** Payloads
carry claims (batch ids, actions), never amounts.

The executors live in [`everstrat-xyz/contracts`](https://github.com/everstrat-xyz/contracts).

---

## The hard constraints

These are not style preferences. Each one, violated, produces a keeper that is
either silently broken or actively dangerous.

### 1. A payload must never carry an authoritative amount

No ETH amount, NAV, or price. Params are claims and hints only.

The executors re-derive everything from live state — `QueueKeeperExecutor`
re-validates affordability, `StrategyKeeperExecutor` never reads params — and
this repo holds up its end by making amounts inexpressible:

- W1 params are a batch id and an index range, nothing else
  (`mimic-functions/queue-keeper/src/params.ts`).
- `decode()` enforces the **exact** wire length per action, because all
  layouts are static and a smuggled amount can only appear as a trailing word
  that Solidity's `abi.decode` would silently ignore. `function.ts` runs it
  over the bytes it is about to send, so the rule holds on every tick — while
  it was unreachable from `main()` it did not even compile (AssemblyScript has
  no closures), which is what an unenforced guard decays into.

**Reviewing:** any new field in `Params`, any amount reaching `encode()`, any
relaxation of the length check.

### 2. The tick clock is seconds, not milliseconds

Age comparisons (`minBatchAge`, `MAX_BATCH_PROCESSING_TIME`) compare against
`createdAt` / `pricedAt`, which are `block.timestamp`. W1's `state.now` is
Mimic's `context.timestamp` **milliseconds** divided by 1000
(`function.ts` → `readState`). Forgetting the division once skewed every
window by 1000×. That context is the runner's execution time, not the block
the oracle reads landed in — a small skew vs chain is possible; mixing units
is the failure that is not.

**Reviewing:** any `Date.now()` reaching a decision input, or any seconds/ms
mixup at the read boundary.

### 3. W1 may scan deeper than the contract. W2 may not.

`MAX_BATCH_SCAN` equals `MAX_LIVE_PRICED_BATCHES` (25). The view peeks up to
25 skippable ids then scans 25 more — a live batch at about `cursor+26` is
already in view. `_execute(ProcessRequests)` still has no window.

That extra depth does **not** fire with W1 as the only `perform` caller:

- Every successful `perform` (including `PriceBatch`) runs
  `_advanceBatchCursor` (+25 skippable). W1 cannot accumulate a 25-long
  dead prefix.
- If W1 is down, nothing calls `priceBatch`, so `currentBatchId` does not
  grow. Expired live work sits in the window you already had (≤25 +
  unpriced current), it does not pile up in front of new live batches.
- Seeing live work the view cannot requires another `KEEPER_ROLE` to keep
  pricing while this cursor is frozen, *and* the first live id past
  ~`cursor+50`.

`maxBatches` (default 250) can still truncate the off-chain header walk
(`scanTruncatedAt`). That needs `current - cursor ≥ 250`, or a tiny
`maxBatches` (the spec uses 2). Do not treat truncation as a production
cadence event.

W1's `ProcessRequests` prefix uses the same walk as `_affordableRequests`,
capped at `maxUsersPerUpkeep` (default 20). `maxRequestsPerBatch` defaults
to 50. A shorter claim than a *correct* view is not something decide
chooses; it would take `maxRequestsPerBatch < maxUsersPerUpkeep` or an
oracle Controller balance below the view's. The shorter-prefix
`intended-improvement` class exists because the executor accepts any
prefix; the spec mocks a larger on-chain `count`.

W2: `_execute` re-derives with the **same bounded helpers** the view uses.
A truer off-chain shortfall would be rejected — hence the relay.

`AdvanceCursor` is capped at the bounded peek
(`decide.ts` → `peekAdvancedCursor(s, MAX_BATCH_SCAN)`).

### 4. AssemblyScript's `Map.get` aborts the whole module

`Map.get` on a missing key is not `null` — it is `abort: "Key does not
exist"`, which kills the tick. If `maxBatches` stops the header walk short,
ids past that point are absent from the state map, so every access must
probe `has()` first (`decide.ts` → `getBatch`). Same class of trap:
`BigInt.pow` overflows silently past ~2^170 — build `2^256` as `(2^128)^2`
(`params.ts` → `word`).

**Reviewing:** any raw `.get()` on the batch map, any exponent near 256.

### 5. Both functions read; only the intent writes

The single state-changing path in either function is the EvmCall intent it
emits at the end of a tick. Everything else is an oracle-backed read. Anything
that reaches the chain outside that path is new power in the keeper plane and
needs to be seen as such.

---

## Testing conventions

### Golden values come from Solidity, never from the function's own encoder

A round-trip against our own encoder passes no matter how wrong the layout is.
Expected calldata in the specs is built with **ethers' `Interface` from the
executor ABI** (`tests/function.spec.ts`), an independent implementation — not
by re-running the function's `encode`.

Do not "recompute" a fixture from the code under test to make it pass — that
is testing the code against itself.

### The tests run the real compiled WASM

Both functions are tested through a raw-mock harness (`tests/helpers.ts`)
that keys oracle responses by the same EIP-712 query hash the runner
computes, so the WASM runs unmodified — no re-implemented read layer to drift.
The harness's `debug` flag lists any read the mocks failed to cover.

### Contract semantics are transcribed, not reimplemented

`src/solmath.ts` mirrors the contracts' `Math` library, including truncating
division and the strict `<` in `isRelativelyLessThan`. The affordability walk
**breaks** at the first request that overruns the balance — it does not skip an
expensive request to fit cheaper ones behind it.

"Close enough" produces a keeper that proposes work the contract refuses, which
looks exactly like a broken keeper.

### Enum ordinals are pinned

`Action.None/PriceBatch/ProcessRequests/AdvanceCursor` = 0/1/2/3, matching
`IQueueKeeperExecutor.QueueAction`. Solidity enums reorder silently; the
constants in `decide.ts` must move with them or every payload is silently
retargeted.

### Divergence classification is code, not judgment

W1 cross-checks its decision against the on-chain view every tick and logs a
class: `match`, `intended-improvement`, `truncated-scan`, or `bug`. With
defaults and W1 as the only performer, production ticks should be `match`.
The other two explained classes are for a mocked or mis-set view/scan cap
(shorter prefix, `maxBatches: 2`) or a frozen cursor plus another pricer
(batch past ~`cursor+50`). Shadow-mode graduation was "zero unexplained
divergences over 7 days" — "explained" is defined in `src/divergence.ts`.
A new source of benign disagreement needs a new class, not a shrug.

---

## Repo mechanics

- **`make check`** = lint plus compile + mocha for both Mimic functions. There
  is no Go in this repo any more; `make install` bootstraps both functions.
- **The Mimic CLI's `mimic test` spawns a tsx IPC pipe** that sandboxed
  environments block; run it (and `npm test`) outside restricted sandboxes.
- **Vendored ABIs are never hand-edited.** Each function's `abis/` is refreshed
  from `forge build` output in `everstrat-xyz/contracts` per
  [`mimic-functions/ABIS.md`](mimic-functions/ABIS.md); update the pinned
  commit in the same PR. `Pausable.json` is a hand-written exception,
  documented there. An ABI that no `manifest.yaml` lists should be deleted —
  it compiles into nothing and implies a read the function does not make.
- **Addresses are not secrets.** They are public config. W1's inputs carry the
  executor, Controller, ExitQueue and AMM addresses plus the Mimic smart
  account; W2's carry the executor and the smart account. `manifest.yaml` is
  the authoritative list — `scripts/create-trigger.ts` and
  `docs/MIMIC_CUTOVER.md` must name every key it declares, or trigger creation
  fails manifest validation.
- **The Mimic smart account is looked up in the Protocol App (per chain)
  before a live trigger is signed.** It is a function input (`.addUser`)
  *and* the address ADMIN passes to `allowExecutorCaller` — the same value.
  Dry-run / prefill may use `0x0` because they do not settle; a live trigger
  with `0x0` would target the zero address. The allowlist is settable because
  that account is not known at executor deploy, and a recreated task can
  rotate it. Executors deploy inert by design. See `docs/MIMIC_CUTOVER.md`.

### CI

A job that reports success while doing nothing is worse than no job. A CRE
simulate job once did exactly that for months — gated on a secret it did not
need — and hid both missing coverage and a broken CLI install. It is gone with
W4, and the rule it taught outlives it: never let an absent secret, or a job
that skipped, look like a pass.

What remains is one matrix job compiling each function and running its specs
against the compiled artifact. Note what it does **not** cover: no read path is
exercised against a real chain, so ABI drift is caught by the vendored files,
not by execution.

---

## Style

- Comments explain **why**, not what. The what is in the code; the why is the
  contract behaviour or platform constraint that forced the shape.
- Errors name the field and the consequence, not just the failure.
- Mirror the surrounding code's density and idiom.
- Prefer making a mistake inexpressible over documenting that it is forbidden.
