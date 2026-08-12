# Vendored ABIs — provenance

These JSON files are the `abi` field of `forge build` artifacts from
[`everstrat-xyz/contracts`](https://github.com/everstrat-xyz/contracts), copied
verbatim (keys sorted, so diffs stay readable). **Do not hand-edit them** — a
local edit silently desynchronises W1/W2 from the deployed receivers.

| Field | Value |
| --- | --- |
| Source repo | `everstrat-xyz/contracts` |
| Commit | `b9caf2e9c9c50a4a6c2640dfc33f0da19b515b48` |
| Branch at vendoring time | `feat/cre-keeper-executors` (PR for contracts [#4](https://github.com/everstrat-xyz/contracts/issues/4) / [#5](https://github.com/everstrat-xyz/contracts/pull/5)) |
| Vendored on | 2026-08-12 |

> The receiver contracts had not landed on `contracts@main` when these were
> vendored. Re-run the refresh below once they merge, and again before the
> Sepolia cutover ([issue #6](https://github.com/everstrat-xyz/keepers/issues/6)).

## Contents

| File | Why the keepers need it |
| --- | --- |
| `ICREReceiverBase.json` | `Envelope` shape, identity setters, `lastSequence`, `MAX_REPORT_AGE`, `CHAIN_SELECTOR` |
| `ICREQueueExecutor.json` | W1 target: `queueUpkeepStatus`, `nextLiveBatchIdToProcess`, `affordableRequests`, `minBatchAge`, `maxUsersPerUpkeep` |
| `ICREStrategyExecutor.json` | W2 target: `strategyUpkeepStatus`, `pendingRedemptionNeedsETH`, thresholds |
| `IRegistry.json` | `getContractByKey` — address resolution for every protocol contract |
| `IController.json` | Controller balance / pause state cross-checks |
| `IExitQueue.json` | Off-chain full-queue scan: `currentBatchId`, `batchInfo`, `unprocessedUsers*`, `requestInfo`, `MAX_BATCH_PROCESSING_TIME` |
| `IAMM.json` | Pause state and exit-liquidity reads |
| `IStrategyManager.json` | Rebalance / deposit-capacity / performance-fee reads |

## Refreshing

```bash
CONTRACTS=../contracts   # path to a clean everstrat-xyz/contracts checkout
(cd "$CONTRACTS" && forge build)

for f in ICREReceiverBase ICREQueueExecutor ICREStrategyExecutor \
         IRegistry IController IExitQueue IAMM IStrategyManager; do
  jq -S '.abi' "$CONTRACTS/out/$f.sol/$f.json" > "contracts/evm/src/abi/$f.json"
done

go test ./...   # envelope/params fixtures must still pass
```

Then update the commit hash in the table above in the same PR.

## Typed bindings

`cre generate-bindings evm` (which writes generated Go clients into
`contracts/evm/src/`) is deliberately **not** run yet: it pulls the
`cre-sdk-go/capabilities/blockchain/evm` module, which only earns its place once
W1/W2 actually issue on-chain reads ([#3](https://github.com/everstrat-xyz/keepers/issues/3) /
[#4](https://github.com/everstrat-xyz/keepers/issues/4)). Until then `abi.Get`
gives packing/unpacking against the same JSON the generator would consume.
