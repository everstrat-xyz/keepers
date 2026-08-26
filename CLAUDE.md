# CLAUDE.md — EverStrat Keepers

Guidance for Claude Code working in this repo, and the reference the automated
PR reviewer reads. Everything here is a rule that has already cost something to
learn; the "why" matters more than the rule.

## What this repo is

Automation for EverStrat's keeper plane, running on the
[Mimic Network](https://mimic.fi):

- **W1** (`mimic-functions/queue-keeper/`) — a Mimic **Solidity Function**
  (AssemblyScript compiled to WASM) driving `QueueKeeperExecutor`. It scans the
  exit queue deeper than the gas-bounded on-chain view and submits `perform()`
  calldata through an oracle-signed EvmCall intent.
- **W2** (`mimic-functions/strategy-keeper/`) — a thin Mimic relay function.
  `StrategyKeeperExecutor` exposes its own `checker()` returning
  `(canExec, execPayload)`; the function forwards the payload **verbatim** —
  no off-chain re-derivation, no payload interpretation.
W4 (`freeze-watch/`), the read-only freeze-precursor watcher, was removed
along with the Go toolchain and CRE CLI it was the last consumer of. It is in
git history if it comes back.

Governing principle, from `TECH_SPEC.md` §5:

> **Workflows orchestrate. Contracts decide.**

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

### 2. The clock is the observed block's, never the wall clock

Age comparisons (`minBatchAge`, `MAX_BATCH_PROCESSING_TIME`) use
`block.timestamp` of the block the state was read at. The wall clock can sit
ahead of the chain, and a batch's `createdAt` was recorded from
`block.timestamp` — comparing the two compares different clocks, and would
fail every tick.

In W1 that means `state.now` derives from the runner's tick timestamp
converted to seconds (`function.ts` → `readState`); nothing else. The runner
hands the function a **millisecond** timestamp while every contract timestamp
is in seconds — forgetting the division once skewed every window by 1000×.

**Reviewing:** any `Date.now()` reaching a decision input, or any seconds/ms
mixup at the read boundary.

### 3. W1 may scan deeper than the contract. W2 may not.

This asymmetry is in the **contracts**, and it is the single easiest thing to
get wrong:

- `QueueKeeperExecutor._processReport` validates a `ProcessRequests` claim
  **per batch, with no scan window**. So W1 scanning past the on-chain view's
  25-batch window is a genuine win — the executor still accepts it.
- `StrategyKeeperExecutor._processReport` re-derives every quantity with the
  **same bounded helpers** the view uses. That is precisely why W2 is a pure
  relay: a truer off-chain shortfall would be rejected on arrival.

`AdvanceCursor` is W1's exception: the executor advances the cursor with its
*bounded* walk, so the claim is capped at what one execution can reach
(`decide.ts` → `peekAdvancedCursor(s, MAX_BATCH_SCAN)`).

### 4. AssemblyScript's `Map.get` aborts the whole module

`Map.get` on a missing key is not `null` — it is `abort: "Key does not
exist"`, which kills the tick. W1's scan can be truncated
(`inputs.maxBatches`), leaving ids past the truncation point absent from the
state map, so every access must probe `has()` first (`decide.ts` →
`getBatch`). Same class of trap: `BigInt.pow` overflows silently past
~2^170 — build `2^256` as `(2^128)^2` (`params.ts` → `word`).

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
class: `match`, `intended-improvement` (beyond the view's scan window),
`truncated-scan`, or `bug`. Shadow-mode graduation was "zero unexplained
divergences over 7 days" — "explained" is defined in `src/divergence.ts`, not
argued per incident. A new source of benign disagreement needs a new class
with a reason, not a shrug.

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
- **The Mimic smart-account signer is assigned when the task is created**,
  which is why `KeeperExecutorBase` has a settable allowlist rather than an
  immutable constructor arg. Executors deploy inert by design.

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
