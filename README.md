# EverStrat Keepers (Mimic)

Automation for EverStrat's keeper plane, on the [Mimic Network](https://mimic.fi).

The Chainlink CRE workflows this repo was built on were retired when Chainlink
Automation was sunset and CRE turned out not to be permissionless; a brief
Gelato interlude ended the same way when Gelato sunset its automation product.
W1 and W2 now run as Mimic functions, and they are all this repo holds.

| Component | Role | On-chain target | Model |
| --- | --- | --- | --- |
| `mimic-functions/queue-keeper/` | W1 — exit-queue automation | `QueueKeeperExecutor` | Mimic **Solidity Function** (deep scan off-chain) |
| `mimic-functions/strategy-keeper/` | W2 — strategy automation | `StrategyKeeperExecutor` | Mimic function relaying the contract's own `checker()` |

**W4 (freeze-watch)** — the read-only freeze-precursor and keeper-health
watcher — was a CRE-era Go workflow and has been **removed** rather than
carried along unmigrated: it was the last thing keeping a Go toolchain, a CRE
CLI dependency, and a `cre workflow simulate` CI job in a repo that is
otherwise pure AssemblyScript. It is recoverable from git history
(`git log -- freeze-watch/`), and its observability belongs in whatever
monitoring stack replaces it rather than in the keeper plane.

## Why the split

W1's whole value is scanning **deeper than the gas-bounded on-chain view**:
`QueueKeeperExecutor.queueUpkeepStatus()` stops after `MAX_BATCH_SCAN` (25)
batches, while the function walks cursor→current with no such cap, then
claims a batch the view could not reach. `perform()` re-validates per batch
with no window applied, so the claim is accepted.

W2's decisions are all bounded re-derivations — there is no depth to gain — so
the contract's own `checker()` stays authoritative and the function only
relays its `execPayload` verbatim.

Both executors authenticate the same way: Mimic calls `perform()` from a
dedicated smart-account signer, which must be in the executor's
`allowExecutorCaller` allowlist. An empty allowlist means `perform()` always
reverts (`KeeperExecutorNoAllowedCallers`) — the executors deploy inert and
only act once a task is created and its signer bound.

## Prerequisites

- **Node** `20+`
- A [Mimic](https://mimic.fi) account, funded

## Local checks

```bash
make install   # npm install in both functions
make check     # lint + compile + specs for both functions
make test      # compile + specs only

# Or directly:
cd mimic-functions/queue-keeper
npm install
npm test
```

## Deploying W1 (queue-keeper function)

The full sequence — task creation, signer discovery, allowlist binding, and
verification — is the runbook: [`docs/MIMIC_CUTOVER.md`](docs/MIMIC_CUTOVER.md).

Short version:

1. Deploy the executors (`DeployKeeperExecutors` in
   `everstrat-xyz/contracts`) — they come up **inert** (no allowed callers).
2. `mimic deploy` the `queue-keeper` function and create its task (time-based
   trigger).
3. Read the task's dedicated signer address from the Mimic app.
4. `allowExecutorCaller(signer)` on `QueueKeeperExecutor` (ADMIN_ROLE).
5. Verify: the run log shows `divergence=match` (or
   `intended-improvement`), and a dry `perform()` from the signer path
   succeeds.

W2 deploys the same way: `mimic deploy` the `strategy-keeper` function,
create its task, allowlist the signer.

## Repo layout

```text
├── mimic-functions/
│   ├── queue-keeper/       # W1 — deep-scan Mimic function (AssemblyScript)
│   │   ├── src/function.ts # tick: read state, decide, emit intent
│   │   ├── src/decide.ts   # decision engine (pure)
│   │   ├── src/params.ts   # payload encoder — batch id + index range only
│   │   └── tests/          # raw-mock oracle harness + scenario specs
│   ├── strategy-keeper/    # W2 — checker() relay, payload forwarded verbatim
│   │   ├── src/function.ts # tick: read checker(), relay execPayload
│   │   └── tests/
│   └── ABIS.md             # Vendored-ABI provenance and refresh recipe
├── docs/MIMIC_CUTOVER.md   # The cutover runbook
└── blueprints/             # Design notes (01 — system overview)
```

## The address book

Protocol addresses are **function inputs**, declared in each `manifest.yaml`:
W1 takes the executor, Controller, ExitQueue and AMM; W2 takes the executor.
Both also take the Mimic smart account that will call `perform()`.

The AMM is there for one reason — its pause flag. `queueUpkeepStatus` refuses
to recommend work while the AMM is paused, and W1 has to refuse for the same
reason: `Controller.priceBatch` is `whenNotPaused` on the Controller alone, so
an AMM-only pause would not stop the transaction.

Because the addresses are inputs rather than Registry lookups, a redeploy that
re-registers a contract needs the triggers updated. That is the trade for not
paying a Registry round-trip per tick against a timelocked address book.

## Hard constraints carried over from the CRE era

**A payload must never carry an authoritative amount.** No ETH amount, NAV, or
price — params are claims and hints only. The executors re-derive everything
from live state; the discipline exists so a future edit cannot start trusting a
value it should verify:

- W1 params are a batch id and an index range, nothing else
  (`mimic-functions/queue-keeper/src/params.ts`).
- `decode()` enforces the **exact** wire length per action, because all
  layouts are static and a smuggled amount can only appear as a trailing word
  that Solidity's `abi.decode` would silently ignore. `function.ts` runs it
  over the bytes it is about to send, so the rule holds on every tick rather
  than only in tests.

**The clock is the observed block's.** Age comparisons use the tick timestamp
of the state read, converted to seconds — never a wall clock, and never the
runner's raw milliseconds (`mimic-functions/queue-keeper/src/function.ts`,
`readState`). Mixing units skews every window by 1000×.

**W1 may scan deeper than the contract; W2 may not.** This asymmetry is in the
contracts: `QueueKeeperExecutor._processReport` validates a `ProcessRequests`
claim per batch with no window (so the deep scan is a genuine win), while
`StrategyKeeperExecutor` re-derives quantities with the same bounded helpers
the view uses — hence W2's verbatim relay.

## Testing conventions

### Expected calldata comes from ethers, never from the function's encoder

A round-trip against our own encoder passes no matter how wrong the layout is.
The specs build expected calldata with ethers' `Interface` over the executor
ABI — an independent implementation (`mimic-functions/*/tests/function.spec.ts`).

### The tests run the real compiled WASM

Both functions are exercised through a raw-mock harness
(`mimic-functions/*/tests/helpers.ts`) that keys oracle responses by the same
EIP-712 query hash the runner computes, so the compiled function runs
unmodified. A mock-schema limitation in `@mimicprotocol/test-ts` (no tuple
returns) is why the harness exists; its `debug` flag lists unmocked reads.

### Contract semantics are transcribed, not reimplemented

`src/solmath.ts` mirrors the contracts' `Math` library including truncating
division and the strict `<` in `isRelativelyLessThan`. The affordability walk
breaks at the first request that overruns the balance — it does not skip an
expensive request to fit cheaper ones behind it. "Close enough" produces a
keeper that proposes work the contract refuses.

### Enum ordinals are pinned

`Action.None/PriceBatch/ProcessRequests/AdvanceCursor` are 0/1/2/3, matching
`IQueueKeeperExecutor.QueueAction`. Reordering the Solidity enum without
updating the constants silently retargets every payload — pinned by the
specs' exact-calldata assertions.

## CI baseline

GitHub Actions (`.github/workflows/ci.yml`) runs one job, **functions**, as a
matrix over `queue-keeper` and `strategy-keeper`: `npm ci`, lint, then `npm
test` — which compiles each function to WASM and runs its specs against that
artifact.

Nothing here exercises the read path against a real chain. The specs mock the
oracle, so a signature drift is caught by the vendored ABIs (see
[`mimic-functions/ABIS.md`](mimic-functions/ABIS.md)) rather than by
execution. A forked-deployment e2e needs a Mimic runner pointed at an anvil
fork and is not built yet.

## Automated PR review

`.github/workflows/claude-code-review.yml` reviews every PR with Claude Code,
mirroring `everstrat-xyz/contracts`. The review prompt targets this repo's
specific failure modes; [`CLAUDE.md`](CLAUDE.md) holds the reasoning and is
the standard the reviewer applies.

## Useful links

- [Mimic docs](https://mimic.fi) — Protocol App, developer guides
- Contracts: `QueueKeeperExecutor`, `StrategyKeeperExecutor` in `everstrat-xyz/contracts`
