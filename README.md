# EverStrat Keepers (CRE)

Go workflows for the [Chainlink Runtime Environment (CRE)](https://docs.chain.link/cre) that drive EverStrat’s keeper plane:

| Package | Role | On-chain consumer |
| --- | --- | --- |
| `queue-keeper/` | W1 — exit-queue automation | `CREQueueExecutor` |
| `strategy-keeper/` | W2 — strategy automation | `CREStrategyExecutor` |

Language is **Go** (WASM / `wasip1`). Business logic is intentionally out of scope here — this repo bootstrap makes tooling, layout, and docs usable so W1/W2 can be implemented next.

Design context: EverStrat monorepo `TECH_SPEC.md` §5 / §7. Receivers live in [`everstrat-xyz/contracts`](https://github.com/everstrat-xyz/contracts).

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
cre workflow simulate queue-keeper --target staging-settings
cre workflow simulate strategy-keeper --target staging-settings
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
├── project.yaml           # RPC targets (staging / production)
├── secrets.yaml           # Secret name → env var map
├── .env.example
├── go.mod                 # Single module for all workflows
├── contracts/evm/src/
│   ├── abi/               # Drop ABIs here; `cre generate-bindings evm`
│   └── keystone/          # Keystone-related artifacts
├── queue-keeper/          # W1 scaffold
└── strategy-keeper/       # W2 scaffold
```

Shared Envelope encode/decode + ABI helpers: [issue #2](https://github.com/everstrat-xyz/keepers/issues/2).

## CI baseline

GitHub Actions (`.github/workflows/ci.yml`):

1. **Always:** `go vet` / `go test` on non-WASM packages (when added) + module tidy check.
2. **Simulate:** `cre workflow simulate` for `queue-keeper` and `strategy-keeper`, gated on `CRE_ETH_PRIVATE_KEY` (and optional `CRE_API_KEY`) so PRs without secrets still get compile/vet signal.

Full simulate/lint matrix expansion: [issue #8](https://github.com/everstrat-xyz/keepers/issues/8).

## Useful links

- [CRE docs](https://docs.chain.link/cre)
- [Project configuration (Go)](https://docs.chain.link/cre/reference/project-configuration-go)
- [Forwarder Directory](https://docs.chain.link/cre/guides/workflow/using-evm-client/forwarder-directory-go)
- [Deploying workflows](https://docs.chain.link/cre/guides/operations/deploying-workflows)
- Contracts CRE receivers: `CREQueueExecutor`, `CREStrategyExecutor` in `everstrat-xyz/contracts`
