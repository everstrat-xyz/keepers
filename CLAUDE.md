# CLAUDE.md — EverStrat Keepers

Guidance for Claude Code working in this repo, and the reference the automated
PR reviewer reads. Everything here is a rule that has already cost something to
learn; the "why" matters more than the rule.

## What this repo is

Automation for EverStrat's keeper plane, running on the Gelato Network:

- **W1** (`web3-functions/queue-keeper/`) — a Gelato **TypeScript Web3
  Function** driving `QueueKeeperExecutor`. It scans the exit queue deeper
  than the gas-bounded on-chain view and submits `perform()` calldata.
- **W2** — no code here at all. `StrategyKeeperExecutor` exposes its own
  `checker()`, and Gelato calls it directly as a solidity resolver.
- **W4** (`freeze-watch/`) — a CRE-era Go workflow, observability only, **no
  writes**. Its Gelato migration is deferred.

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
  (`web3-functions/queue-keeper/src/params.ts`).
- `decode()` enforces the **exact** wire length per action, because all
  layouts are static and a smuggled amount can only appear as a trailing word
  that Solidity's `abi.decode` would silently ignore.

**Reviewing:** any new field in `Params`, any amount reaching `encode()`, any
relaxation of the length check.

### 2. The clock is the observed block's, never `Date.now()`

Age comparisons (`minBatchAge`, `MAX_BATCH_PROCESSING_TIME`) use
`block.timestamp` of the block the state was read at. The wall clock can sit
ahead of the chain, and a batch's `createdAt` was recorded from
`block.timestamp` — comparing the two compares different clocks, and would
fail every tick.

In W1 that means `state.now` comes from `provider.getBlock("latest")`, and
nothing else.

**Reviewing:** any `Date.now()` / `new Date()` reaching a decision input.

### 3. W1 may scan deeper than the contract. W2 may not.

This asymmetry is in the **contracts**, and it is the single easiest thing to
get wrong:

- `QueueKeeperExecutor._processReport` validates a `ProcessRequests` claim
  **per batch, with no scan window**. So W1 scanning past the on-chain view's
  25-batch window is a genuine win — the executor still accepts it.
- `StrategyKeeperExecutor._processReport` re-derives every quantity with the
  **same bounded helpers** the view uses. That is precisely why W2 has no
  off-chain code: a truer off-chain shortfall would be rejected on arrival.

`AdvanceCursor` is W1's exception: the executor advances the cursor with its
*bounded* walk, so the claim is capped at what one execution can reach
(`decide.ts` → `peekAdvancedCursor(s, MAX_BATCH_SCAN)`).

### 4. ethers v5 → bigint, through one coercion point

The Gelato SDK pins ethers v5; `Contract` calls return `BigNumber` typed as
`any`, while the decision engine works in native `bigint`. Mixing the two
either throws or compares unequal without warning.

All conversion goes through the `w()` helper at the read boundary in
`index.ts`. A `BigNumber` reaching `decide()` cannot happen silently.

**Reviewing:** any read result assigned to a `State`/`Batch`/`Request` field
without going through `w()`.

### 5. W4 cannot write

`freeze-watch/` has no code path from an Alert to a transaction. That is the
guarantee — actuation would require adding code a reviewer can see. NAV
guardian actuation is a separate epic behind DAO sign-off.

---

## Testing conventions

### Golden values come from Solidity, never from TS

A round-trip against our own encoder passes no matter how wrong the layout is.
The TS fixtures reuse the exact hex the retired Go suite generated with
`cast abi-encode` and `chisel` (a real Solidity evaluator):

- `src/params.test.ts` — ABI-encoded params bytes
- `src/solmath.test.ts` — `convertAssets` / `isRelativelyLessThan` including
  truncating division and boundary strictness

Do not "recompute" a fixture in TS to make it pass — that is testing the code
against itself.

### Contract semantics are transcribed, not reimplemented

`src/solmath.ts` mirrors the contracts' `Math` library, including truncating
division and the strict `<` in `isRelativelyLessThan`. The affordability walk
**breaks** at the first request that overruns the balance — it does not skip an
expensive request to fit cheaper ones behind it.

"Close enough" produces a keeper that proposes work the contract refuses, which
looks exactly like a broken keeper.

### Enum ordinals are pinned

`Action.None/PriceBatch/ProcessRequests/AdvanceCursor` = 0/1/2/3, matching
`IQueueKeeperExecutor.QueueAction`. Solidity enums reorder silently; the TS
enum must move with them or every payload is silently retargeted.

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
- **`make check`** = Go fmt+vet+lint+test+build, plus the Web3 Function
  typecheck and jest.
- **`npm install --ignore-scripts`** locally — `deno-bin`'s postinstall fetches
  a runtime from GitHub releases that is not needed for typecheck/jest.
- **Vendored ABIs are never hand-edited.** Refresh from `forge build` output in
  `everstrat-xyz/contracts` per
  [`contracts/evm/src/abi/SOURCE.md`](contracts/evm/src/abi/SOURCE.md) and
  update the pinned commit in the same PR. `Pausable.json` and
  `Multicall3.json` are hand-written exceptions, documented there.
- **Addresses are not secrets.** They are public config. Gelato task user-args
  carry `registryAddress` and the executor address; nothing else.
- **The Gelato dedicated proxy is assigned at task creation**, which is why
  `KeeperExecutorBase` has a settable allowlist rather than an immutable
  constructor arg. Executors deploy inert by design.

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
