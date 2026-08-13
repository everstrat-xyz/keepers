# EverStrat Keepers (CRE)

Go workflows for the [Chainlink Runtime Environment (CRE)](https://docs.chain.link/cre) that drive EverStrat’s keeper plane:

| Package | Role | On-chain consumer |
| --- | --- | --- |
| `queue-keeper/` | W1 — exit-queue automation | `CREQueueExecutor` |
| `strategy-keeper/` | W2 — strategy automation | `CREStrategyExecutor` |
| `pkg/` | Shared Envelope / report / chain-config code used by both | — |

Language is **Go** (WASM / `wasip1`). W1 and W2 are both implemented and run in **shadow mode** — they decide an action each tick and cross-check it against the on-chain view, but do not write. Live `writeReport` stays off until the Sepolia cutover ([#6](https://github.com/everstrat-xyz/keepers/issues/6)).

## Prerequisites

- **Go** `1.25.3+` (`go version`)
- **CRE CLI** (documented against **v1.29.0**; newer is fine)
- **CRE account** in the EverStrat org ([create / join](https://docs.chain.link/cre/account))
- Optional for local simulate: any EOA private key in `.env` (Sepolia ETH only needed once you broadcast / write onchain)

### Install CRE CLI (macOS / Linux)

```bash
curl -sSL https://app.chain.link/cre/install.sh | bash
cre version
```

Manual install: download the matching asset from [cre-cli releases](https://github.com/smartcontractkit/cre-cli/releases), verify SHA-256, rename to `cre`, and put it on your `PATH`. Details: [CLI installation](https://docs.chain.link/cre/getting-started/cli-installation/macos-linux).

### Authenticate

```bash
cre login
cre whoami
```

CI / headless: set `CRE_API_KEY` (Organization → APIs in the CRE UI; requires deploy Early Access). See [Authentication](https://docs.chain.link/cre/reference/cli/authentication).

### Confirm org enablement (required once)

Authoritative chain + mock-forwarder list for **your** tenant:

```bash
cre workflow supported-chains
cre workflow supported-chains --output json   # scripting
cre account access                            # deploy Early Access status
cre registry list                             # private vs onchain registries
```

If `supported-chains` fails, the org is not enabled for workflows yet — fix that before implementing W1/W2.

## KeystoneForwarder addresses

Re-verified from the [Forwarder Directory](https://docs.chain.link/cre/guides/workflow/using-evm-client/forwarder-directory-go) (docs last updated **2026-06-24**). **Do not trust stale tables** — re-check the directory and `cre workflow supported-chains` before deploying receivers.

| Network | CRE chain name | Production `KeystoneForwarder` | Simulation `MockKeystoneForwarder` |
| --- | --- | --- | --- |
| Ethereum Sepolia | `ethereum-testnet-sepolia` | `0xF8344CFd5c43616a4366C34E3EEE75af79a74482` | `0xF8344CFd5c43616a4366C34E3EEE75af79a74482` |
| Ethereum Mainnet | `ethereum-mainnet` | `0x0b93082D9b3C7C97fAcd250082899BAcf3af3885` | `0x0b93082D9b3C7C97fAcd250082899BAcf3af3885` |

CCIP chain selectors used in workflow config / `CREReceiverBase`:

| Network | `chainSelector` (decimal string) |
| --- | --- |
| Ethereum Sepolia | `16015286601757825753` |
| Ethereum Mainnet | `5009297550715157269` |

Pass the **production** forwarder into `CREQueueExecutor` / `CREStrategyExecutor` constructors (`KEYSTONE_FORWARDER` in contracts deploy scripts). Simulation consumers that validate `msg.sender` against a forwarder must use the mock address from `supported-chains` for that tenant.

Both tables are compiled into [`pkg/chains`](pkg/chains/chains.go) and pinned by unit tests, so a config that names a chain gets the right selector and forwarder without retyping them. Keep the two in sync when the directory changes.

> Older samples sometimes cite Sepolia mock `0x15fC6ae953E024d975e77382eEeC56A9101f9F88`. That address is **not** what the current Forwarder Directory lists for Sepolia — treat it as stale unless your org’s `supported-chains` output says otherwise.

## Fresh clone → first simulate

```bash
git clone https://github.com/everstrat-xyz/keepers.git
cd keepers

# Tooling
go version   # >= 1.25.3
cre version
cre login && cre whoami
cre workflow supported-chains

# Config
cp .env.example .env
# set CRE_ETH_PRIVATE_KEY (64 hex chars, no 0x) and SEPOLIA_RPC_URL

go mod tidy

# Scaffold ticks (no writeReport yet)
cre workflow simulate queue-keeper --target staging-settings --trigger-index 0
cre workflow simulate strategy-keeper --target staging-settings --trigger-index 0
```

Point configs at deployed contracts by editing `queue-keeper/config.*.json` and `strategy-keeper/config.*.json`:

- `registryAddress` — EverStrat `Registry` proxy
- `queueExecutorAddress` — `CREQueueExecutor`
- `strategyExecutorAddress` — `CREStrategyExecutor`
- keep `shadowMode: true` until the Sepolia cutover issue enables `writeReport`

## Funding & spend model

CRE workflow **execution** is metered in **credits** (see Total Workflow Spend / Credits Used in the [CRE workflows UI](https://app.chain.link/cre/workflows)). Monitor spend after deploy; W4 will alert on low balance / silent stop.

Separately you may need gas:

| What | Funding |
| --- | --- |
| Local `cre workflow simulate` | Private key in `.env` (no gas if not broadcasting) |
| Deploy / lifecycle on **private** registry | CRE session only — no ETH for registry ops |
| Deploy / lifecycle on **onchain** registry (`onchain:ethereum-mainnet`) | Linked key (`cre account link-key`) + **ETH on Ethereum Mainnet** for Workflow Registry txs |
| Onchain `writeReport` delivery | DON path via KeystoneForwarder; consumer contracts must already be deployed and bound |

Historical Automation used LINK upkeep balances; CRE’s UI bills **credits**. Keep the org funded/credited before relying on live keepers.

## Secrets

1. Declare names in `secrets.yaml` (`secretsNames`).
2. For simulation: put values in `.env` / the environment.
3. For deployed workflows: `cre secrets create|update|list|delete` against the Vault DON.

Workflow code uses `runtime.GetSecret()` either way. See [Managing Secrets](https://docs.chain.link/cre/guides/workflow/secrets).

## Deploy (shadow / private registry)

Default `workflow.yaml` targets use `deployment-registry: "private"` (no linked wallet / Mainnet gas for registry ops).

```bash
cre workflow deploy queue-keeper --target staging-settings
cre workflow deploy strategy-keeper --target staging-settings

cre workflow get queue-keeper --target staging-settings
cre workflow pause queue-keeper --target staging-settings   # as needed
cre workflow activate queue-keeper --target staging-settings
```

Onchain registry + identity binding for live `writeReport` are covered by the Sepolia cutover issue — leave `shadowMode: true` until then.

## Repo layout

```text
.
├── project.yaml           # RPC targets (local / staging / production)
├── secrets.yaml           # Secret name → env var map
├── .env.example
├── go.mod                 # Single module for all workflows
├── Makefile               # make check = vet + test + wasip1 build
├── docs/
│   ├── envelope.md        # Envelope rules W1/W2 must obey
│   ├── READ_BUDGET.md     # CRE's 15-read limit and how W1/W2 fit it
│   └── LOCAL_FORK.md      # Run a workflow against a real deployment
├── scripts/                # Solidity-derived fixture generators
│                           #   (cast / chisel produce the golden values)
├── contracts/evm/src/
│   ├── abi/               # Vendored ABIs + Go accessors (see SOURCE.md)
│   └── keystone/          # Keystone-related artifacts
├── pkg/
│   ├── envelope/          # abi.encode(Envelope) codec + staleness guards
│   ├── queue/             # W1 actions + params (no amounts)
│   ├── strategy/          # W2 actions (action-only reports)
│   ├── keystone/          # Workflow-identity metadata helpers
│   ├── solmath/           # Mirrors of the contracts' Math library
│   ├── evmread/           # CRE reads: ABI, Multicall3 batching, read budget
│   ├── crewrite/          # DON-signed writeReport delivery
│   ├── chains/            # Per-chain constants + config validation
│   └── registry/          # Registry keys and role identifiers
├── queue-keeper/          # W1 — implemented, shadow mode
└── strategy-keeper/       # W2 — implemented, shadow mode
```

## The address book

`pkg/registry` is the one place to get a contract address or its ABI, mirroring
how nothing on-chain stores its peers' addresses — it holds the Registry and
asks it. Only `registryAddress` is configured; everything else is derived.

```go
p, err := registry.Resolve(caller, registryAddress,
    registry.Controller, registry.ExitQueue, registry.AMM)

exitQueue, _ := p.ExitQueue()
calls := []evmread.SubCall{
    exitQueue.Sub("currentBatchId"),   // address and ABI travel together
    exitQueue.Paused(),                // Pausable is inherited, so it has its own builder
}
```

Two properties worth keeping:

- **One chain read** for the whole book, however many keys. Use `ResolveWith`
  to fold your own independent reads into the same one — against a budget of 15
  per execution, a spare round trip is expensive.
- **Address and ABI cannot be mis-paired.** A raw `(address, abiName)` call site
  can send the ExitQueue's address the Controller's selectors; the call then
  reverts somewhere far from the mistake. A `Contract` carries both.

The key → ABI mapping is asserted once, in `TestBoundABIsAreVendoredAndUsable`,
rather than re-derived at each call site.

## Shared packages

Both workflows encode reports through the same code, so W1 and W2 cannot drift
from each other or from `CREReceiverBase`.

| Package | What it gives you |
| --- | --- |
| `pkg/envelope` | `abi.encode(Envelope)` codec, plus `Validate` / `NextSequence` / `Deadline` mirroring the receiver's chain, replay and staleness guards |
| `pkg/queue` | W1's decision engine: affordability model, cursor logic, params encoders, divergence classification |
| `pkg/strategy` | W2's decision engine: priority order, redemption cost model, action-only `Report.Build` |
| `pkg/keystone` | Workflow name → `bytes10`, 64-byte metadata encode/decode, and a binding pre-flight check for the cutover |
| `pkg/chains` | Chain selectors and forwarder addresses, plus `Resolve` to validate a workflow's `config.*.json` |
| `pkg/registry` | The protocol address book — the Go mirror of `Registry.sol` + `Auth.sol`. Resolves every contract from the Registry in one chain read and binds each address to its ABI |
| `pkg/solmath` | Transcriptions of the contracts' `Math` library, so off-chain affordability matches on-chain to the wei |
| `pkg/evmread` | CRE EVM reads with ABI packing, Multicall3 batching, and the read budget |
| `pkg/crewrite` | DON-signed `writeReport` delivery, shared by W1/W2 |
| `contracts/evm/src/abi` | Vendored contract ABIs, parsed on demand |

**Hard constraint:** a report must never carry an authoritative ETH amount, NAV,
or price — params are claims and hints only. The APIs above are shaped so that
is not expressible, and unit tests reject amount-bearing params.

Read [`docs/envelope.md`](docs/envelope.md) and [`docs/READ_BUDGET.md`](docs/READ_BUDGET.md) before writing W1/W2 logic: it
covers the `sequence`, `observedAt` and `MAX_REPORT_AGE` rules, the
`ProcessRequests` prefix constraint, and the identity-binding rules.

### Local checks

```bash
make check      # go vet + go test (host) + wasip1 build (workflows)
make test
make fixtures   # regenerate Solidity-derived fixtures (needs Foundry + jq)
```

Unit tests cover the decision logic, but only the **local fork harness**
exercises the EVM read path against a real deployment — see
[`docs/LOCAL_FORK.md`](docs/LOCAL_FORK.md). It is what caught CRE's 15-read
limit and the block-timestamp-vs-wall-clock bug; run it before trusting a change
to `queue-keeper/reads.go`.

```bash
cre workflow simulate queue-keeper --target local-settings --non-interactive --trigger-index 0
```

Note that `go test ./...` does **not** work from a host toolchain: the workflow
mains are `//go:build wasip1`, so those directories have no buildable files on
linux/darwin. Use `./pkg/... ./contracts/...`, as the Makefile and CI do.

## CI baseline

GitHub Actions (`.github/workflows/ci.yml`):

1. **Always:** module tidy check, `go vet` + `go test` on the host packages (`./pkg/...`, `./contracts/...`), `gofmt`, and a `wasip1` vet + build of both workflows.
2. **Simulate:** `cre workflow simulate` for `queue-keeper` and `strategy-keeper`, gated on `CRE_ETH_PRIVATE_KEY` (and optional `CRE_API_KEY`) so PRs without secrets still get compile/vet signal.

Full simulate/lint matrix expansion: [issue #8](https://github.com/everstrat-xyz/keepers/issues/8).

## Useful links

- [CRE docs](https://docs.chain.link/cre)
- [Project configuration (Go)](https://docs.chain.link/cre/reference/project-configuration-go)
- [Forwarder Directory](https://docs.chain.link/cre/guides/workflow/using-evm-client/forwarder-directory-go)
- [Deploying workflows](https://docs.chain.link/cre/guides/operations/deploying-workflows)
- Contracts CRE receivers: `CREQueueExecutor`, `CREStrategyExecutor` in `everstrat-xyz/contracts`
