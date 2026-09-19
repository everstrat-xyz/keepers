# Vendored ABIs — provenance

Each function carries its own `abis/` directory, listed in its `manifest.yaml`
and compiled into the generated wrappers under `src/types/`. The JSON is the
`abi` field of `forge build` artifacts from
[`everstrat-xyz/contracts`](https://github.com/everstrat-xyz/contracts), copied
verbatim (keys sorted, so diffs stay readable).

**Do not hand-edit them.** A local edit silently desynchronises a function from
the deployed executor, and the failure shows up as a reverting `perform()`, not
as a build error.

| Field | Value |
| --- | --- |
| Source repo | `everstrat-xyz/contracts` |
| Commit | `49c0e63` (main) — UniCLStrat calm/TWAP surface for keepers#27 suppression |
| Vendored on | 2026-09-19 |

## Contents

`mimic-functions/queue-keeper/abis/` (W1):

| File | Why the function needs it |
| --- | --- |
| `QueueKeeperExecutor.json` | The W1 target: `perform`, `queueUpkeepStatus`, `nextBatchIdToProcess`, `minBatchAge`, `maxUsersPerUpkeep`, `paused` |
| `IExitQueue.json` | The off-chain queue scan: `currentBatchId`, `batchInfo`, `unprocessedUsers*`, `requestInfo`, `MAX_BATCH_PROCESSING_TIME` |
| `Pausable.json` | Pause fan-out on the executor, Controller, ExitQueue and AMM |

`mimic-functions/strategy-keeper/abis/` (W2):

| File | Why the function needs it |
| --- | --- |
| `StrategyKeeperExecutor.json` | The W2 target: `checker()` in, `perform` calldata out; `registry()` for the suppression walk |
| `IRegistry.json` | `getContractByKey(STRATEGY_MANAGER)` — the registry has no named getter |
| `StrategyManager.json` | `strategies()` — the executor's Rebalance selection set |
| `UniCLStrat.json` | Per-strategy suppression reads: `paused`, `isHealthy`, `pool`, `maxTickDeviation`, `twapInterval`, `shortTwapInterval` (public state the interfaces do not declare) |
| `IUniswapV3Pool.json` | `_isCalm()` inputs: `slot0` spot tick and `observe` tick cumulatives |

The **contract** ABIs are vendored where the function reads public state
(`minBatchAge`, `nextBatchIdToProcess`, `maxTickDeviation`) that the
interfaces do not declare. Interfaces are vendored where they are the whole
surface: `IExitQueue`, `IRegistry` (`getContractByKey` is the only read), and
`IUniswapV3Pool` (the pools are external Uniswap deployments — no pool
contract exists in the repo to vendor).

An ABI that no `manifest.yaml` lists is dead weight and should be deleted: it
is compiled into nothing, and its presence implies a read the function does not
make.

## Refreshing

```bash
CONTRACTS=../contracts   # path to a clean everstrat-xyz/contracts checkout
(cd "$CONTRACTS" && forge build)

jq -S '.abi' "$CONTRACTS/out/QueueKeeperExecutor.sol/QueueKeeperExecutor.json" \
  > mimic-functions/queue-keeper/abis/QueueKeeperExecutor.json
jq -S '.abi' "$CONTRACTS/out/IExitQueue.sol/IExitQueue.json" \
  > mimic-functions/queue-keeper/abis/IExitQueue.json
jq -S '.abi' "$CONTRACTS/out/StrategyKeeperExecutor.sol/StrategyKeeperExecutor.json" \
  > mimic-functions/strategy-keeper/abis/StrategyKeeperExecutor.json
jq -S '.abi' "$CONTRACTS/out/IRegistry.sol/IRegistry.json" \
  > mimic-functions/strategy-keeper/abis/IRegistry.json
jq -S '.abi' "$CONTRACTS/out/StrategyManager.sol/StrategyManager.json" \
  > mimic-functions/strategy-keeper/abis/StrategyManager.json
jq -S '.abi' "$CONTRACTS/out/UniCLStrat.sol/UniCLStrat.json" \
  > mimic-functions/strategy-keeper/abis/UniCLStrat.json
jq -S '.abi' "$CONTRACTS/out/IUniswapV3Pool.sol/IUniswapV3Pool.json" \
  > mimic-functions/strategy-keeper/abis/IUniswapV3Pool.json

(cd mimic-functions/queue-keeper && npx mimic codegen)
(cd mimic-functions/strategy-keeper && npx mimic codegen)
make test   # `make functions` is an alias for this; it does not codegen
```

Then update the commit hash in the table above in the same PR. The specs build
their expected calldata from these files through ethers' `Interface`, so a
signature that moved without a refresh fails the suite rather than reaching a
chain.

## Hand-written fragments

`Pausable.json` is **not** vendored from EverStrat's build and is not covered by
the refresh above: it is OpenZeppelin's `paused()`. The EverStrat interfaces
inherit `Pausable` without re-declaring it, so it appears in no forge artifact,
yet W1's pause fan-out reads it on the Controller, the ExitQueue and the AMM.
Controller ETH balance is `environment.getNativeTokenBalance`, not an ABI.
