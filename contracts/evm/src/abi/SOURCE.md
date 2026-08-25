# Vendored ABIs — provenance

These JSON files are the `abi` field of `forge build` artifacts from
[`everstrat-xyz/contracts`](https://github.com/everstrat-xyz/contracts), copied
verbatim (keys sorted, so diffs stay readable). **Do not hand-edit them** — a
local edit silently desynchronises the keepers from the deployed executors.

| Field | Value |
| --- | --- |
| Source repo | `everstrat-xyz/contracts` |
| Commit | `<update on refresh>` — Gelato-era executor rename (`IKeeperExecutorBase`, `IQueueKeeperExecutor`, `IStrategyKeeperExecutor`) |
| Vendored on | 2026-08-25 |

> The Gelato migration renamed the CRE receivers and replaced the
> forwarder/identity surface (`onReport`, `expectedWorkflowId`, `expectedAuthor`)
> with a caller allowlist (`allowExecutorCaller`, `isExecutorCaller`,
> `executorCallerCount`) and a `checker()`/`perform()` Gelato surface. Refresh
> again before cutover if contracts move.

## Contents

| File | Why the keepers need it |
| --- | --- |
| `IKeeperExecutorBase.json` | Caller-allowlist surface: `allowExecutorCaller`, `isExecutorCaller`, `executorCallerCount`, `pause`/`unpause` |
| `IQueueKeeperExecutor.json` | W1 target: `checker`, `perform`, `queueUpkeepStatus`, `nextLiveBatchIdToProcess`, `affordableRequests`, `minBatchAge`, `maxUsersPerUpkeep` |
| `IStrategyKeeperExecutor.json` | W2 target: `checker`, `perform`, `strategyUpkeepStatus`, `pendingRedemptionNeedsETH`, thresholds |
| `IRegistry.json` | `getContractByKey` — address resolution for every protocol contract |
| `IController.json` | Controller balance / pause state cross-checks |
| `IExitQueue.json` | Off-chain full-queue scan: `currentBatchId`, `batchInfo`, `unprocessedUsers*`, `requestInfo`, `MAX_BATCH_PROCESSING_TIME` |
| `IAMM.json` | Pause state and exit-liquidity reads |
| `IStrategyManager.json` | Strategy list, deposit cooldown, performance-fee reads |
| `IStrategy.json` | Per-strategy health, pause, max deposit/withdrawal |
| `IOracle.json` | Feed freshness — the Registry address book binds it to the `ORACLE` key |

## Refreshing

```bash
CONTRACTS=../contracts   # path to a clean everstrat-xyz/contracts checkout
(cd "$CONTRACTS" && forge build)

for f in IKeeperExecutorBase IQueueKeeperExecutor IStrategyKeeperExecutor \
         IRegistry IController IExitQueue IAMM IStrategyManager IStrategy IOracle; do
  jq -S '.abi' "$CONTRACTS/out/$f.sol/$f.json" > "contracts/evm/src/abi/$f.json"
done

go test ./pkg/... ./contracts/...   # surface pins must still pass
```

Then update the commit hash in the table above in the same PR.

## Hand-written fragments

`Pausable.json` and `Multicall3.json` are **not** vendored from EverStrat's
build and are not covered by the refresh above:

- `Pausable.json` — OpenZeppelin's `paused()`. The EverStrat interfaces inherit
  Pausable without re-declaring it, so it appears in no forge artifact, yet both
  `*UpkeepStatus` views gate on it.
- `Multicall3.json` — the canonical aggregator's `aggregate3`. W4 still runs on
  CRE, which caps a workflow at 15 contract reads per execution, so batched
  reads are the only way to scan anything there.
