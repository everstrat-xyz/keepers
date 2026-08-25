# EverStrat Keepers (Gelato)

Automation for EverStrat's keeper plane, on the [Gelato Network](https://gelato.network).

The Chainlink CRE workflows this repo was built on were retired when Chainlink
Automation was sunset and CRE turned out not to be permissionless. W1 and W2
now run as Gelato tasks; W4 remains a CRE-era Go workflow pending its own
migration (deferred).

| Component | Role | On-chain target | Model |
| --- | --- | --- | --- |
| `web3-functions/queue-keeper/` | W1 — exit-queue automation | `QueueKeeperExecutor` | Gelato **TypeScript Web3 Function** (deep scan off-chain) |
| — (on-chain) | W2 — strategy automation | `StrategyKeeperExecutor` | Gelato **solidity resolver** — `checker()` lives on the contract |
| `freeze-watch/` | W4 — freeze precursors and keeper health (read-only) | — | CRE workflow (Go, deferred) |
| `pkg/` | Shared Go code used by W4 | — | — |

## Why the split

W1's whole value is scanning **deeper than the gas-bounded on-chain view**:
`QueueKeeperExecutor.queueUpkeepStatus()` stops after `MAX_BATCH_SCAN` (25)
batches, while the Web3 Function walks cursor→current with no such cap, then
claims a batch the view could not reach. `perform()` re-validates per batch
with no window applied, so the claim is accepted.

W2's decisions are all bounded re-derivations — there is no depth to gain — so
it uses the contract's own `checker()` and needs no off-chain code at all.

Both executors authenticate the same way: Gelato calls `perform()` from a
dedicated proxy address, which must be in the executor's `allowExecutorCaller`
allowlist. An empty allowlist means `perform()` always reverts
(`KeeperExecutorNoAllowedCallers`) — the executors deploy inert and only act
once a task is created and its proxy bound.

## Prerequisites

- **Node** `20+` for the Web3 Function (`web3-functions/queue-keeper`)
- **Go** `1.25.3+` for W4 and `pkg/`
- A [Gelato](https://app.gelato.network) account with 1Balance funded
- **CRE CLI** (`cre`) — only for W4 simulate, until its migration

## Local checks

```bash
make check     # Go: fmt + vet + lint + test + wasip1 build; TS: typecheck + jest
make test      # Go tests only
make w3f       # Web3 Function typecheck + tests

# Or directly:
cd web3-functions/queue-keeper
npm install --ignore-scripts   # deno-bin's postinstall is not needed locally
npm run typecheck
npm test
```

## Deploying W1 (queue-keeper Web3 Function)

The full sequence — task creation, proxy discovery, allowlist binding, and
verification — is the runbook: [`docs/GELATO_CUTOVER.md`](docs/GELATO_CUTOVER.md).

Short version:

1. Deploy the executors (`DeployKeeperExecutors` in
   `everstrat-xyz/contracts`) — they come up **inert** (no allowed callers).
2. Create the Gelato task (`web3-functions/queue-keeper`, time-based trigger).
3. Read the task's dedicated proxy address from the Gelato dashboard.
4. `allowExecutorCaller(proxy)` on `QueueKeeperExecutor` (ADMIN_ROLE).
5. Verify: `checker()`/simulate shows canExec, and a dry `perform()` from the
   proxy path succeeds.

W2 needs no task-side code: create a resolver task pointed at
`StrategyKeeperExecutor.checker()` and allowlist its proxy the same way.

## Repo layout

```text
├── web3-functions/
│   └── queue-keeper/      # W1 — Gelato TypeScript Web3 Function
│       ├── index.ts        # tick: read state, decide, return calldata
│       └── src/            # decision engine (pure, unit-tested)
├── freeze-watch/           # W4 — CRE-era Go workflow (deferred)
├── pkg/
│   ├── chains/             # Per-chain constants + config validation
│   ├── evmread/            # CRE reads: ABI, Multicall3 batching, budget (W4)
│   ├── registry/           # Registry keys and role identifiers
│   └── freezewatch/        # W4 alert thresholds and payloads
├── contracts/evm/src/abi/  # Vendored contract ABIs + Go accessors
├── docs/
│   ├── GELATO_CUTOVER.md   # The cutover runbook
│   └── LOCAL_FORK.md       # Run W4 against a real deployment
└── blueprints/             # Design notes (W4 retained; W1/W2 historical
                            #  CRE blueprints removed with the Go workflows)
```

## The address book

Only `registryAddress` is configured; every other protocol address is resolved
from the Registry at tick time, so a redeploy that re-registers a contract
cannot leave the keeper pointed at a dead address.

The same rule holds in the Web3 Function (`index.ts`): `controller`, `exitQueue`
and `amm` all come from `registry.getContractByKey(...)`, keyed by
`keccak256("CONTROLLER")` etc. — the same constants `Auth.sol` uses.

## Hard constraints carried over from the CRE era

**A payload must never carry an authoritative amount.** No ETH amount, NAV, or
price — params are claims and hints only. The executors re-derive everything
from live state; the discipline exists so a future edit cannot start trusting a
value it should verify:

- W1 params are a batch id and an index range, nothing else
  (`web3-functions/queue-keeper/src/params.ts`).
- `decode()` enforces the **exact** wire length per action, because all
  layouts are static and a smuggled amount can only appear as a trailing word
  that Solidity's `abi.decode` would silently ignore.

**The clock is the observed block's.** Age comparisons use `block.timestamp`
of the block the state was read at, never `Date.now()` — the wall clock can
sit ahead of the chain, and minBatchAge checks against it would fail every
tick.

**W1 may scan deeper than the contract; W2 may not.** This asymmetry is in the
contracts: `QueueKeeperExecutor._processReport` validates a `ProcessRequests`
claim per batch with no window (so the deep scan is a genuine win), while
`StrategyKeeperExecutor` re-derives quantities with the same bounded helpers
the view uses.

**W4 cannot write.** `freeze-watch/` has no code path from an Alert to a
report. That guarantee is an import away from breaking, which is exactly why
it is spelled out here.

## Testing conventions

### Golden values come from Solidity, never from TS

A round-trip against our own encoder passes no matter how wrong the layout is.
The TS test fixtures reuse the exact hex values the Go suite generated with
`cast abi-encode` / `chisel` (a real Solidity evaluator) — see the comments in
`src/solmath.test.ts` and `src/params.test.ts`.

### Contract semantics are transcribed, not reimplemented

`src/solmath.ts` mirrors the contracts' `Math` library including truncating
division and the strict `<` in `isRelativelyLessThan`. The affordability walk
breaks at the first request that overruns the balance — it does not skip an
expensive request to fit cheaper ones behind it. "Close enough" produces a
keeper that proposes work the contract refuses.

### Enum ordinals are pinned

`Action.None/PriceBatch/ProcessRequests/AdvanceCursor` are 0/1/2/3, matching
`IQueueKeeperExecutor.QueueAction`. Reordering the Solidity enum without
updating the TS enum silently retargets every payload — pinned by
`src/params.test.ts`.

### ethers v5 → bigint, one coercion point

The Gelato SDK pins ethers v5, whose `Contract` calls return `BigNumber`. The
decision engine works in native `bigint`. All conversion goes through one
`w()` helper at the read boundary in `index.ts`, so a `BigNumber` leaking
into `decide()` cannot happen silently.

## CI baseline

GitHub Actions (`.github/workflows/ci.yml`):

1. **go** — module tidy check, vet + `-race` tests on `pkg/...`/`contracts/...`,
   a wasip1 build of freeze-watch with the CRE 20 MB compressed-size check,
   and gofmt.
2. **lint** — golangci-lint v2, host pass plus a wasip1 pass for freeze-watch.
3. **w3f** — `npm install --ignore-scripts`, typecheck, and jest for the
   queue-keeper Web3 Function.
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

- [Gelato Web3 Functions docs](https://docs.gelato.network/web3-functions)
- [Gelato dedicated msg.sender](https://docs.gelato.network/developer-products/web3-functions/quick-start/advanced/dedicated-msg-sender)
- Contracts: `QueueKeeperExecutor`, `StrategyKeeperExecutor` in `everstrat-xyz/contracts`
