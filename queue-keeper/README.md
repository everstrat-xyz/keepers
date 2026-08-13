# W1 — queue-keeper

Scaffold only. Cron stub ready for shadow-mode queue logic (`CREQueueExecutor`).

See the repo root [README](../README.md) for simulate / deploy / fund steps.

Business logic: tracked in [issue #3](https://github.com/everstrat-xyz/keepers/issues/3).

## What already exists

The tick validates `config.<target>.json` through
[`pkg/chains`](../pkg/chains) and logs the resolved deployment. While the
configs still carry zero-address placeholders it logs the resolve failure and
returns `bound: false` rather than erroring — shadow mode only. Outside shadow
mode an unresolvable config fails the run.

Report encoding is done and tested in [`pkg/queue`](../pkg/queue):

```go
r := queue.Report{ChainSelector: sel, Sequence: seq, ObservedAt: observedAt}
report, err := r.ProcessRequests(batchID, endIndex) // endIndex exclusive
```

What issue #3 still owes: the on-chain reads (`lastSequence()`,
`queueUpkeepStatus()`, the off-chain full-queue scan) that decide *which* action
to propose, and the `writeReport` call itself.

## Rules to read first

[`docs/envelope.md`](../docs/envelope.md) — in particular:

- read `lastSequence()` every run; never keep a local counter
- `startIndex` must be `0`; the receiver accepts only a prefix of the affordable
  set, so a shorter claim is fine and an offset one always reverts
- `params` carry a batch id and an index range — **never** an ETH amount
