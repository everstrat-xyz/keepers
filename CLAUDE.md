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
- **W4** (`freeze-watch/`) — a CRE-era Go workflow, observability only, **no
  writes**. Its own migration is deferred.

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
  that Solidity's `abi.decode` would silently ignore.

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

### 5. W4 cannot write

`freeze-watch/` has no code path from an Alert to a transaction. That is the
guarantee — actuation would require adding code a reviewer can see. NAV
guardian actuation is a separate epic behind DAO sign-off.

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

- **`go test ./...` does not work** — freeze-watch's main is
  `//go:build wasip1`, so a host toolchain excludes it. Use `./pkg/...
  ./contracts/...` for host packages, as the Makefile does.
- **`make check`** = Go fmt+vet+lint+test+build, plus both Mimic functions'
  compile + mocha.
- **The Mimic CLI's `mimic test` spawns a tsx IPC pipe** that sandboxed
  environments block; run it (and `npm test`) outside restricted sandboxes.
- **Vendored ABIs are never hand-edited.** Refresh from `forge build` output in
  `everstrat-xyz/contracts` per
  [`contracts/evm/src/abi/SOURCE.md`](contracts/evm/src/abi/SOURCE.md) and
  update the pinned commit in the same PR. `Pausable.json` and
  `Multicall3.json` are hand-written exceptions, documented there.
- **Addresses are not secrets.** They are public config. The functions' inputs
  carry `registryAddress` and the executor address; nothing else.
- **The Mimic smart-account signer is assigned when the task is created**,
  which is why `KeeperExecutorBase` has a settable allowlist rather than an
  immutable constructor arg. Executors deploy inert by design.

### CI

A job that reports success while doing nothing is worse than no job. The
simulate job once did exactly that for months — gated on a secret it did not
need — and hid both missing coverage and a broken CLI install.

So: gate on the secret actually required, and make skips **loud** (warning
annotation plus run-summary note). Never let an absent secret look like a pass.

---

## Style

- Comments explain **why**, not what. The what is in the code; the why is the
  contract behaviour or platform constraint that forced the shape.
- Errors name the field and the consequence, not just the failure.
- Mirror the surrounding code's density and idiom.
- Prefer making a mistake inexpressible over documenting that it is forbidden.
