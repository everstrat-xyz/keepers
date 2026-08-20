# Report Envelope — rules the workflows must obey

The `report` bytes a keeper workflow hands to `writeReport` are
`abi.encode(ICREReceiverBase.Envelope)`:

```solidity
struct Envelope {
    uint64 chainSelector;
    uint64 sequence;
    uint64 observedAt;
    uint8  action;
    bytes  params;
}
```

`CREReceiverBase.onReport` runs the Chainlink `ReceiverTemplate` auth checks
(forwarder → workflow id / author / name), then decodes this Envelope and
applies three guards of its own before dispatching to `_processReport`. Every
guard is a `revert`: a report that breaks a rule burns credits and delivers
nothing. It cannot corrupt state — that is the design — but a keeper that
routinely trips one is a keeper that is silently not working.

Go implementation: [`pkg/envelope`](../pkg/envelope). The guards below are
mirrored by `Envelope.Validate`, so a workflow can refuse to emit a report the
contract would reject.

---

## 1. `chainSelector`

**Rule:** must equal the receiver's immutable `CHAIN_SELECTOR`, or
`CREReceiverWrongChain`.

The value is a **CCIP chain selector**, not an EVM chain id — Sepolia is
`16015286601757825753`, not `11155111`. Both are compiled into
[`pkg/chains`](../pkg/chains/chains.go); `chains.Resolve` fails a config whose
`chainSelector` does not match its `chainName`, which is the realistic failure
(copying a config between environments and updating only one field).

`CHAIN_SELECTOR` is a constructor immutable. If it is wrong on a deployed
receiver, the fix is a redeploy — there is no setter.

## 2. `sequence`

**Rule:** must be **strictly greater** than the receiver's `lastSequence`, or
`CREReceiverReplayedSequence`. On success the receiver sets
`lastSequence = sequence`.

This is per-receiver replay protection, and it has one non-obvious consequence:

> **Read `lastSequence()` from the receiver on every run. Do not keep a local
> counter.**

`lastSequence` is state the workflow does not exclusively own. A manual
break-glass report from the multisig `KEEPER_ROLE` path, a redeployed workflow
starting from zero, or two workflow versions overlapping during a rollout all
move it independently. A workflow that trusts its own counter will sit below the
receiver and have every report rejected, indefinitely and silently.

`envelope.NextSequence(lastSequence)` returns the lowest acceptable value.
Sequence numbers are not required to be dense — gaps are fine, and jumping ahead
is safe. Only going backwards or repeating is fatal.

Because the receiver only advances `lastSequence` on a **successful** report,
a reverted delivery does not consume a number.

## 3. `observedAt` and `MAX_REPORT_AGE`

**Rule:** `observedAt <= block.timestamp` and
`block.timestamp - observedAt <= MAX_REPORT_AGE`, or `CREReceiverStaleReport`.

`observedAt` is the unix second at which the workflow observed the state its
report claims — **not** the time the report was built, and not the time it is
delivered.

> **Take it from the observed block, never from `runtime.Now()`.**
>
> `runtime.Now()` is the DON's wall clock. The receiver compares `observedAt`
> against `block.timestamp` with **zero** tolerance in the future direction, so
> a DON clock even one second ahead of the chain rejects *every* report. Reading
> the block's own timestamp (`evmread.BlockTimestamp`) guarantees `observedAt`
> is behind the delivering block rather than racing it.
>
> The same applies to any age comparison against chain state: a batch's
> `createdAt` was recorded from `block.timestamp`, so comparing it to wall-clock
> time is comparing two different clocks. This was caught on the local fork
> harness ([docs/LOCAL_FORK.md](LOCAL_FORK.md)) when W1 disagreed with
> `queueUpkeepStatus` about whether a batch had reached `minBatchAge`.

Two failure directions:

- **Future-dated.** Any `observedAt` above the delivering block's timestamp
  reverts, with zero tolerance. Do not add slack "to be safe"; clock skew
  between the DON and the chain makes this a real way to reject every report.
- **Stale.** Delivery lands some blocks after the workflow runs, so the budget
  the workflow gets is `MAX_REPORT_AGE` *minus* the DON's consensus and
  transmission latency. Treat the configured age as a ceiling to stay well
  inside, not a target.

`MAX_REPORT_AGE` is a constructor immutable (non-zero, enforced). The
`maxReportAgeSeconds` field in `config.*.json` is a **workflow-side mirror** of
it and can drift from what is deployed — read `MAX_REPORT_AGE()` on-chain before
making a staleness decision that matters. `pkg/chains` additionally rejects a
configured age above 24h (`chains.MaxReportAgeCeiling`), since an age measured
in days would let a long-stalled workflow deliver an observation that no longer
describes reality.

Helpers: `envelope.Deadline(observedAt, maxReportAge)` and
`envelope.RemainingBudget(observedAt, maxReportAge, now)`. Do not emit a report
whose remaining budget the DON cannot plausibly beat.

## 4. `action` and `params`

`action` is the receiver's enum ordinal:

| Receiver | Actions |
| --- | --- |
| `CREQueueExecutor` | 0 `None`, 1 `PriceBatch`, 2 `ProcessRequests`, 3 `AdvanceCursor` |
| `CREStrategyExecutor` | 0 `None`, 1 `Rebalance`, 2 `WithdrawShortfall`, 3 `DepositExcess`, 4 `HarvestPerformanceFees`, 5 `Sync`, 6 `ProvideExitLiquidity` |

`None` is the enum's zero value and is what the `*UpkeepStatus` views return
when there is nothing to do. It is **never** a valid report action —
`_processReport` reverts `KeeperExecutorUnknownAction`. Ordinals are pinned by
unit tests in [`pkg/queue`](../pkg/queue) and [`pkg/strategy`](../pkg/strategy);
reordering the Solidity enums without updating them would silently retarget
every report.

`params` layouts:

| Action | `params` |
| --- | --- |
| `PriceBatch` | `abi.encode(uint256 batchId)` |
| `ProcessRequests` | `abi.encode(uint256 batchId, uint256 startIndex, uint256 endIndex)` — `endIndex` **exclusive** |
| `AdvanceCursor` | `abi.encode(uint256 batchId)` |
| every `StrategyAction` | empty — `_processReport` ignores params entirely |

`ProcessRequests` has an extra constraint the contract enforces: `startIndex`
must be `0`. The receiver re-derives the affordable set from live state and
accepts only a **prefix** of it, so a workflow may claim a shorter range
(`endIndex` below the affordable count) but never an offset one.
`queue.EncodeProcessRequestsParams` therefore does not take a `startIndex`
argument at all.

---

## The hard constraint: no amounts

> A report must never carry an authoritative ETH amount, NAV, or price. Params
> are claims and hints only.

This is not a style preference. The trust chain is
`CRE DON → KeystoneForwarder → CRE*Executor → Controller`, and the executor
holds `KEEPER_ROLE`. If an amount in a report were authoritative, a workflow bug
would become a settlement bug. The governing principle from `TECH_SPEC.md` §5:

> **Workflows orchestrate. Contracts decide.**

How the contracts hold up their end:

- `CREStrategyExecutor._processReport` takes `bytes memory /* params */` and
  never reads it. Shortfall, excess, top-up and fee amounts are all recomputed
  from live Controller / StrategyManager / ExitQueue / AMM state.
- `CREQueueExecutor` re-validates every claim: `batchId` must be the current
  batch for `PriceBatch`, the range must be an affordable prefix for
  `ProcessRequests`, and the cursor must actually be advanceable for
  `AdvanceCursor`. Anything else is `KeeperExecutorNoUpkeepNeeded`.

How this repo holds up its end:

- `pkg/queue`'s params API accepts only batch ids and an end index, all
  `uint64`. There is no function that takes an amount.
- `pkg/strategy`'s `Report.Build` takes an action and nothing else, and
  `ValidateParams` rejects any non-empty blob.
- `queue.DecodeParams` enforces the **exact** wire length per action. Since all
  three layouts are static, a smuggled amount can only appear as an extra
  trailing word — which Solidity's `abi.decode` would silently ignore. It is
  rejected here instead. Tested in
  [`TestDecodeParamsRejectsSmuggledAmounts`](../pkg/queue/queue_test.go).

A workflow bug must degrade to **no upkeep**, never to wrong settlement.

---

## Encoding parity

Golden report bytes in [`pkg/envelope/testdata/fixtures.json`](../pkg/envelope/testdata/fixtures.json)
are generated by `cast abi-encode` — Foundry's Solidity encoder, i.e. the other
side of the ABI boundary — via
[`scripts/gen-envelope-fixtures.sh`](../scripts/gen-envelope-fixtures.sh). A
round-trip test against Go's own encoder would pass no matter how wrong the
layout was, which is why the fixtures come from outside.

Regenerate after any change to the Solidity `Envelope`:

```bash
./scripts/gen-envelope-fixtures.sh
go test ./...
```

The ABIs those tests encode against are vendored from `everstrat-xyz/contracts`
at a pinned commit — see
[`contracts/evm/src/abi/SOURCE.md`](../contracts/evm/src/abi/SOURCE.md).

## Workflow identity binding

Envelope guards only run **after** the `ReceiverTemplate` identity checks, and
those have their own rules:

- The receiver is **inert until bound** — `onReport` reverts
  `CREReceiverWorkflowUnbound` unless `expectedWorkflowId` or `expectedAuthor`
  is set.
- Workflow **name** validation requires **author** validation. `bytes10` is a
  40-bit truncation of a hash and is not an authorisation on its own; setting a
  name without an author reverts `WorkflowNameRequiresAuthorValidation`.
- `expectedWorkflowName` is `sha256(name)` → lowercase hex → **first 10
  characters** as ASCII, not 10 raw hash bytes. Prefer
  `setExpectedWorkflowName(string)` and use
  `keystone.WorkflowNameBytes10` to predict and verify the stored value.
- Metadata is 62 bytes of identity plus a trailer; production deliveries are 64.
  The receiver requires `length >= 62`.

Helpers and the binding pre-flight check live in
[`pkg/keystone`](../pkg/keystone). The binding itself is part of the Sepolia
cutover, [issue #6](https://github.com/everstrat-xyz/keepers/issues/6).

## Pre-flight checklist

Before a workflow emits a report:

1. `chainSelector` — from `pkg/chains`, matched to the receiver's
   `CHAIN_SELECTOR()`.
2. `sequence` — `envelope.NextSequence(receiver.lastSequence())`, read this run.
3. `observedAt` — the timestamp of the observation, not of the build; deadline
   still comfortably ahead.
4. `action` — never `None`; agrees with the receiver's `*UpkeepStatus` view, or
   the divergence is deliberate and logged (shadow mode,
   [issue #5](https://github.com/everstrat-xyz/keepers/issues/5)).
5. `params` — built through `pkg/queue` / `pkg/strategy`. No amounts.
6. Receiver bound (`expectedWorkflowId` or `expectedAuthor` set) and not paused.
