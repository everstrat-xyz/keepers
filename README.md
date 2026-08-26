# EverStrat Keepers (Mimic)

Automation for EverStrat's keeper plane, on the [Mimic Network](https://mimic.fi).

The Chainlink CRE workflows this repo was built on were retired when Chainlink
Automation was sunset and CRE turned out not to be permissionless; a brief
Gelato interlude ended the same way when Gelato sunset its automation product.
W1 and W2 now run as Mimic functions, and they are all this repo holds.

| Component | Role | On-chain target | Model |
| --- | --- | --- | --- |
| `mimic-functions/queue-keeper/` | W1 — exit-queue automation | `QueueKeeperExecutor` | Mimic function (off-chain decide; same 25-cap as the view) |
| `mimic-functions/strategy-keeper/` | W2 — strategy automation | `StrategyKeeperExecutor` | Mimic function relaying the contract's own `checker()` |

**W4 (freeze-watch)** — the read-only freeze-precursor and keeper-health
watcher — was a CRE-era Go workflow and has been **removed** rather than
carried along unmigrated: it was the last thing keeping a Go toolchain, a CRE
CLI dependency, and a `cre workflow simulate` CI job in a repo that is
otherwise pure AssemblyScript. It is recoverable from git history
(`git log -- freeze-watch/`), and its observability belongs in whatever
monitoring stack replaces it rather than in the keeper plane.

## Why the split

`MAX_BATCH_SCAN` is the same 25 as `ExitQueue.MAX_LIVE_PRICED_BATCHES`.
`priceBatch` will not create a 26th live priced batch. The view peeks up to
25 skippable ids then scans 25 more, so it already sees a live batch at about
`cursor+26`. `_execute(ProcessRequests)` still has no window — a deeper claim
would be accepted — but with W1 as the only performer that depth does not
show up:

- Every `perform` (including `PriceBatch`) runs `_advanceBatchCursor` (+25
  skippable). W1 cannot grow a 25-long dead prefix and leave it sitting.
- If W1 is down, nothing prices, so `currentBatchId` does not grow. The
  window you already had (≤25 live + unpriced current) just expires in place.
- A live batch the view cannot see needs another `KEEPER_ROLE` to keep
  pricing while this cursor is frozen, and the first live id past ~`cursor+50`.
- `maxBatches` default 250: a truncated scan needs `current - cursor ≥ 250`.
  The spec forces it with `maxBatches: 2`.
- Shorter `ProcessRequests` prefixes: W1 uses the same affordability walk as
  the view (`maxUsersPerUpkeep`, default 20). `maxRequestsPerBatch` defaults
  to 50 (looser). The `intended-improvement` shorter-prefix class is for a
  view that reports a larger count; the spec mocks that. Ops would only see
  it if `maxRequestsPerBatch < maxUsersPerUpkeep` or the oracle balance is
  below the view's `address(controller).balance`.

W2's decisions are all bounded re-derivations — a "truer" off-chain number
would revert — so `checker()` stays authoritative and the function only
relays its `execPayload` verbatim.

Both executors authenticate the same way: Mimic calls `perform()` from a
smart account that must be in the executor's `allowExecutorCaller` allowlist
(the same address as the function input `smartAccount`). An empty allowlist
means `perform()` always reverts (`KeeperExecutorNoAllowedCallers`) — the
executors deploy inert. Look the account up in the Mimic app **before**
creating a live trigger; see [`docs/MIMIC_CUTOVER.md`](docs/MIMIC_CUTOVER.md).

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
2. In the Mimic app, copy the smart account for this chain. Put it in
   `.env` / the trigger input `smartAccount`.
3. `mimic deploy` the `queue-keeper` function and create its task with that
   address already filled (time-based trigger). `mimic deploy` only publishes
   WASM; it does not load `.env`.
4. `allowExecutorCaller` of **the same address** on `QueueKeeperExecutor`
   (ADMIN_ROLE).
5. Verify: the run log shows `divergence=match`, and a dry `perform()`
   from that signer succeeds. `intended-improvement` / `truncated-scan`
   are explained classes, not the default W1-only path (see "Why the
   split").

W2 deploys the same way: `mimic deploy` the `strategy-keeper` function,
create its task with the same smart-account input, allowlist that signer.

## Repo layout

```text
├── mimic-functions/
│   ├── queue-keeper/       # W1 — off-chain decide (AssemblyScript)
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

**The tick clock is seconds, not milliseconds.** `createdAt` / `pricedAt` are
`block.timestamp`. W1's `state.now` is Mimic's `context.timestamp` (ms) divided
by 1000 (`function.ts` → `readState`). Forgetting the division skews every
window by 1000×. That context is the runner's execution time, not
`eth_getBlockByNumber` — a small skew vs chain is possible; mixing units is
the failure mode that is not.

**W1 may scan deeper than the contract; W2 may not.** `_execute(ProcessRequests)`
applies no scan window. With W1 as the only performer that depth does not
show up (every `perform` also peeks +25 skippable; a down W1 does not
price). W2 must relay because perform uses the same bounded helpers. See
"Why the split".

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
