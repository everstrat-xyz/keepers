# Vendored ABIs — provenance

These JSON files are the `abi` field of `forge build` artifacts from
[`everstrat-xyz/contracts`](https://github.com/everstrat-xyz/contracts), copied
verbatim (keys sorted, so diffs stay readable). **Do not hand-edit them** — a
local edit silently desynchronises W1/W2 from the deployed receivers.

| Field | Value |
| --- | --- |
| Source repo | `everstrat-xyz/contracts` |
| Commit | `9f29cde9d18c47a966b1b41e59b0ebad52524931` |
| Branch at vendoring time | `main` (includes PRs [#39](https://github.com/everstrat-xyz/contracts/pull/39)–[#43](https://github.com/everstrat-xyz/contracts/pull/43): keeper liability, Controller actuals, deposit-capacity gating) |
| Vendored on | 2026-08-18 |

> The key change in [#43](https://github.com/everstrat-xyz/contracts/pull/43)
> for these ABIs: `IController`'s deposit/withdraw/harvest functions now return
> the StrategyManager actual, and the full-batch `processRequests(batchId)` is a
> no-op on an empty batch. Refresh again before the Sepolia cutover
> ([issue #6](https://github.com/everstrat-xyz/keepers/issues/6)).

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
| `IStrategyManager.json` | Strategy list, deposit cooldown, performance-fee reads |
| `IStrategy.json` | Per-strategy health, pause, max deposit/withdrawal — what W2's priority order turns on |
| `IOracle.json` | Feed freshness — the Registry address book binds it to the `ORACLE` key |

## Refreshing

```bash
CONTRACTS=../contracts   # path to a clean everstrat-xyz/contracts checkout
(cd "$CONTRACTS" && forge build)

for f in ICREReceiverBase ICREQueueExecutor ICREStrategyExecutor \
         IRegistry IController IExitQueue IAMM IStrategyManager IStrategy IOracle; do
  jq -S '.abi' "$CONTRACTS/out/$f.sol/$f.json" > "contracts/evm/src/abi/$f.json"
done

go test ./...   # envelope/params fixtures must still pass
```

Then update the commit hash in the table above in the same PR.

## Hand-written fragments

`Pausable.json` and `Multicall3.json` are **not** vendored from EverStrat's
build and are not covered by the refresh above:

- `Pausable.json` — OpenZeppelin's `paused()`. The EverStrat interfaces inherit
  Pausable without re-declaring it, so it appears in no forge artifact, yet both
  `*UpkeepStatus` views gate on it.
- `Multicall3.json` — the canonical aggregator's `aggregate3`. CRE caps a
  workflow at 15 contract reads per execution, so batched reads are the only way
  to scan anything; see [`docs/READ_BUDGET.md`](../../../../docs/READ_BUDGET.md).

## Typed bindings

`cre generate-bindings evm` (which writes generated Go clients into
`contracts/evm/src/`) is deliberately **not** run yet: it pulls the
`cre-sdk-go/capabilities/blockchain/evm` module, which only earns its place once
W1/W2 actually issue on-chain reads ([#3](https://github.com/everstrat-xyz/keepers/issues/3) /
[#4](https://github.com/everstrat-xyz/keepers/issues/4)). Until then `abi.Get`
gives packing/unpacking against the same JSON the generator would consume.
