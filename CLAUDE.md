# CLAUDE.md — EverStrat Keepers

Guidance for Claude Code working in this repo, and the reference the automated
PR reviewer reads. Everything here is a rule that has already cost something to
learn; the "why" matters more than the rule.

## What this repo is

Go workflows for the Chainlink Runtime Environment (CRE) that drive EverStrat's
keeper plane. They run as WASM (`wasip1`) on a decentralised oracle network,
read protocol state on-chain, and deliver DON-signed reports to receiver
contracts in [`everstrat-xyz/contracts`](https://github.com/everstrat-xyz/contracts).

| Workflow | Role | Receiver |
| --- | --- | --- |
| `queue-keeper/` | W1 — exit-queue automation | `CREQueueExecutor` |
| `strategy-keeper/` | W2 — strategy automation | `CREStrategyExecutor` |
| `freeze-watch/` | W4 — observability, **no writes** | — |

Governing principle, from `TECH_SPEC.md` §5:

> **Workflows orchestrate. Contracts decide.**

---

## The hard constraints

These are not style preferences. Each one, violated, produces a keeper that is
either silently broken or actively dangerous.

### 1. A report must never carry an authoritative amount

No ETH amount, NAV, or price. Params are claims and hints only.

The executor holds `KEEPER_ROLE`. If an amount in a report were authoritative, a
workflow bug would become a settlement bug. The contracts hold up their end —
`CREStrategyExecutor._processReport` never reads params, and `CREQueueExecutor`
re-derives affordability — and this repo holds up its end by making amounts
inexpressible:

- `pkg/queue` params take only batch ids and an end index, all `uint64`.
- `pkg/strategy`'s `Report.Build` takes an action and nothing else.
- `queue.DecodeParams` enforces the **exact** wire length per action, because
  all layouts are static and a smuggled amount can only appear as a trailing
  word that Solidity's `abi.decode` would silently ignore.

**Reviewing:** any new field in a params struct, any `*big.Int` reaching a
report builder, any relaxation of the length check.

### 2. Chain reads are capped at 15 per execution

`ChainRead.CallLimit = 15`, with a 5 kB response cap
(`cre workflow limits export`). Exceeding it aborts the whole execution with
`Public:User:LimitExceeded` — no partial result.

Reads therefore batch through Multicall3 (`pkg/evmread`), which is one chain
read regardless of sub-call count, and every read plan takes from an explicit
`evmread.Budget` before issuing.

**Reviewing:** a `Call`/`Aggregate` inside a loop; a read added to a fixed
preamble without adjusting `TestReadPlanFitsBudget`; a plan that does not
degrade when the budget runs out. Degrading means truncating the scan and
saying so — never aborting the tick.

See [`docs/READ_BUDGET.md`](docs/READ_BUDGET.md).

### 3. The clock is the observed block's, never `runtime.Now()`

`CREReceiverBase` rejects `observedAt > block.timestamp` with **zero**
tolerance. `runtime.Now()` is the DON's wall clock, which can sit ahead of the
chain — so using it would fail *every* report, permanently.

The same applies to any age comparison against chain state: a batch's
`createdAt` was recorded from `block.timestamp`, so comparing it to wall time
compares two different clocks.

Both come from `evmread.BlockTimestamp`.

**Reviewing:** any new `runtime.Now()` outside `Validate`'s delivery-time
argument.

### 4. Sequence comes from the receiver, every tick

`lastSequence` is not state the workflow owns — a break-glass multisig report, a
redeploy, or overlapping workflow versions all move it. A local counter leaves
the keeper permanently behind the receiver with every report rejected.

Always `envelope.NextSequence(receiver.lastSequence())`, read this run.

### 5. W1 may scan deeper than the contract. W2 may not.

This asymmetry is in the **contracts**, not the workflows, and it is the single
easiest thing to get wrong here:

- `CREQueueExecutor._processReport` validates a `ProcessRequests` claim **per
  batch, with no scan window**. So W1 scanning past the on-chain view's 25-batch
  window is a genuine win — the receiver still accepts it.
- `CREStrategyExecutor._processReport` re-derives every quantity with the
  **same bounded helpers** the view uses. So a W2 that computed a truer
  shortfall would propose actions the receiver's own recomputation rejects, and
  revert every time.

`pkg/strategy` therefore mirrors `MAX_BATCH_SCAN` / `MAX_USERS_COST_SCAN`
exactly, pinned by `TestScanCapsMatchTheContract`.

`AdvanceCursor` is W1's exception: the receiver advances with its *bounded*
walk, so the claim is capped at what one report can reach.

### 6. W4 cannot write

`freeze-watch/` imports neither `pkg/crewrite` nor `pkg/envelope`. That is the
guarantee — actuation would require adding an import a reviewer can see. NAV
guardian actuation is a separate epic behind DAO sign-off.

---

## Testing conventions

### Golden values come from Solidity, never from Go

A round-trip against our own encoder passes no matter how wrong the layout is.
Fixtures are generated by Foundry and committed:

- `scripts/gen-envelope-fixtures.sh` — `cast abi-encode`
- `scripts/gen-solmath-fixtures.sh` — `chisel` (a real Solidity evaluator)

**A trap worth knowing:** Solidity constant-folds literal expressions with
*rational* arithmetic, so `chisel eval '(1 * 1) / 1e18'` either fails to compile
or returns the exact value. Every operand must be wrapped in `uint256(...)` to
get EVM integer truncation. The generator does this; hand-written fixtures have
silently tested the wrong thing before.

### Contract semantics are transcribed, not reimplemented

`pkg/solmath` is a literal transcription of the contracts' `Math` library,
including truncating division and the strict `<` in `isRelativelyLessThan`.
Affordability walks the request prefix and **breaks** at the first request that
overruns the balance — it does not skip it to fit cheaper ones behind.

"Close enough" produces a keeper that proposes work the contract refuses, which
looks exactly like a broken keeper.

### Enum ordinals and ABI shapes are pinned

Solidity enums reorder silently. `unprocessedUsers` is overloaded, and
go-ethereum renames the second occurrence to `unprocessedUsers0` — picking the
wrong one fetches every user instead of a bounded prefix. Both are pinned by
tests.

### The fork harness is the only end-to-end check

Unit tests cover decision logic. Only [`docs/LOCAL_FORK.md`](docs/LOCAL_FORK.md)
exercises the EVM read path against a real deployment — it is what caught both
the 15-read limit and the wall-clock bug. Run it before trusting a change to any
`reads.go`.

---

## Repo mechanics

- **`./...` does not work.** The workflow mains are `//go:build wasip1`, so a
  host toolchain excludes every file in them. Use `./pkg/... ./contracts/...`
  for host packages and `GOOS=wasip1` for the workflows, as the Makefile does.
- **`make check`** = vet + lint + test + wasip1 build. Run it before pushing.
- **golangci-lint v2 is required** — v1 refuses to run when built with an older
  Go than this module targets.
- **Vendored ABIs are never hand-edited.** Refresh per
  [`contracts/evm/src/abi/SOURCE.md`](contracts/evm/src/abi/SOURCE.md) and
  update the pinned commit in the same PR. `Pausable.json` and `Multicall3.json`
  are hand-written exceptions, documented there.
- **Addresses are not secrets.** They are public config in
  `config.<target>.json`. `secrets.yaml` is for capabilities only (e.g. the W4
  webhook URL).
- **`shadowMode: true` stays on** until the Sepolia cutover
  ([#6](https://github.com/everstrat-xyz/keepers/issues/6)).

### CI

A job that reports success while doing nothing is worse than no job. The
simulate job did exactly that for months — gated on a secret it did not need —
and hid both its own missing coverage and a CRE CLI install that had been
unpacking the release tarball over the binary.

So: gate on the secret actually required, and make skips **loud** (warning
annotation plus run-summary note). Never let an absent secret look like a pass.

---

## Style

- Comments explain **why**, not what. The what is in the code; the why is the
  contract behaviour or CRE constraint that forced the shape.
- Errors name the field and the consequence, not just the failure.
- Mirror the surrounding code's density and idiom.
- Prefer making a mistake inexpressible over documenting that it is forbidden.
