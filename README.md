# EverStrat Keepers (Mimic)

Automation for EverStrat's keeper plane, on the [Mimic Network](https://mimic.fi).

The Chainlink CRE workflows this repo was built on were retired when Chainlink
Automation was sunset and CRE turned out not to be permissionless; a brief
Gelato interlude ended the same way when Gelato sunset its automation product.
W1 and W2 now run as Mimic functions; W4 remains a CRE-era Go workflow pending
its own migration (deferred).

| Component | Role | On-chain target | Model |
| --- | --- | --- | --- |
| `mimic-functions/queue-keeper/` | W1 — exit-queue automation | `QueueKeeperExecutor` | Mimic **Solidity Function** (deep scan off-chain) |
| `mimic-functions/strategy-keeper/` | W2 — strategy automation | `StrategyKeeperExecutor` | Mimic function relaying the contract's own `checker()` |
| `freeze-watch/` | W4 — freeze precursors and keeper health (read-only) | — | CRE workflow (Go, deferred) |
| `pkg/` | Shared Go code used by W4 | — | — |

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

- **Node** `20+` for the Mimic functions (`mimic-functions/*`)
- **Go** `1.25.3+` for W4 and `pkg/`
- A [Mimic](https://mimic.fi) account, funded
- **CRE CLI** (`cre`) — only for W4 simulate, until its migration

## Local checks

```bash
make check     # Go: fmt + vet + lint + test + wasip1 build; functions: compile + mocha
make test      # Go tests only
make functions # Mimic functions compile + tests

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
│   │   ├── src/decide.ts   # decision engine (pure, unit-tested)
│   │   ├── src/params.ts   # payload encoder — batch id + index range only
│   │   └── tests/          # raw-mock oracle harness + scenario specs
│   └── strategy-keeper/    # W2 — checker() relay, payload forwarded verbatim
├── freeze-watch/           # W4 — CRE-era Go workflow (deferred)
├── pkg/
│   ├── chains/             # Per-chain constants + config validation
│   ├── evmread/            # CRE reads: ABI, Multicall3 batching, budget (W4)
│   ├── registry/           # Registry keys and role identifiers
│   └── freezewatch/        # W4 alert thresholds and payloads
├── contracts/evm/src/abi/  # Vendored contract ABIs + Go accessors
├── docs/
│   ├── MIMIC_CUTOVER.md    # The cutover runbook
│   └── LOCAL_FORK.md       # Run W4 against a real deployment
└── blueprints/             # Design notes (W4 retained; W1/W2 historical
                            #  CRE blueprints removed with the Go workflows)
```

## The address book

Only `registry` is configured; every other protocol address is resolved from
the Registry at tick time where possible, so a redeploy that re-registers a
contract cannot leave the keeper pointed at a dead address.

The same rule holds in the W1 function: `controller` and `exitQueue` come
from `registry.getContractByKey(...)`, keyed by `keccak256("CONTROLLER")` etc.
— the same constants `Auth.sol` uses.

## Hard constraints carried over from the CRE era

**A payload must never carry an authoritative amount.** No ETH amount, NAV, or
price — params are claims and hints only. The executors re-derive everything
from live state; the discipline exists so a future edit cannot start trusting a
value it should verify:

- W1 params are a batch id and an index range, nothing else
  (`mimic-functions/queue-keeper/src/params.ts`).
- `decode()` enforces the **exact** wire length per action, because all
  layouts are static and a smuggled amount can only appear as a trailing word
  that Solidity's `abi.decode` would silently ignore.

**The clock is the observed block's.** Age comparisons use the tick timestamp
of the state read, converted to seconds — never a wall clock, and never the
runner's raw milliseconds (`mimic-functions/queue-keeper/src/function.ts`,
`readState`). Mixing units skews every window by 1000×.

**W1 may scan deeper than the contract; W2 may not.** This asymmetry is in the
contracts: `QueueKeeperExecutor._processReport` validates a `ProcessRequests`
claim per batch with no window (so the deep scan is a genuine win), while
`StrategyKeeperExecutor` re-derives quantities with the same bounded helpers
the view uses — hence W2's verbatim relay.

**W4 cannot write.** `freeze-watch/` has no code path from an Alert to a
report. That guarantee is an import away from breaking, which is exactly why
it is spelled out here.

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

GitHub Actions (`.github/workflows/ci.yml`):

1. **go** — module tidy check, vet + `-race` tests on `pkg/...`/`contracts/...`,
   a wasip1 build of freeze-watch with the CRE 20 MB compressed-size check,
   and gofmt.
2. **lint** — golangci-lint v2, host pass plus a wasip1 pass for freeze-watch.
3. **functions** — `npm install`, compile, and mocha for both Mimic
   functions.
4. **simulate** — freeze-watch through `cre workflow simulate`, gated on
   `CRE_API_KEY`; a skip is a loud warning annotation, never a silent pass.

`fork-e2e.yml` (scheduled/manual) deploys the protocol to an anvil Sepolia
fork, refresh-checks the vendored ABIs against that build, and simulates
freeze-watch against it.

## Automated PR review

`.github/workflows/claude-code-review.yml` reviews every PR with Claude Code,
mirroring `everstrat-xyz/contracts`. The review prompt targets this repo's
specific failure modes; [`CLAUDE.md`](CLAUDE.md) holds the reasoning and is
the standard the reviewer applies.

## Useful links

- [Mimic docs](https://mimic.fi) — Protocol App, developer guides
- Contracts: `QueueKeeperExecutor`, `StrategyKeeperExecutor` in `everstrat-xyz/contracts`
