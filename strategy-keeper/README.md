# W2 — strategy-keeper

Scaffold only. Cron stub ready for shadow-mode strategy logic (`CREStrategyExecutor`).

See the repo root [README](../README.md) for simulate / deploy / fund steps.

Business logic: tracked in [issue #4](https://github.com/everstrat-xyz/keepers/issues/4).

## What already exists

The tick validates `config.<target>.json` through
[`pkg/chains`](../pkg/chains) and logs the resolved deployment. While the
configs still carry zero-address placeholders it logs the resolve failure and
returns `bound: false` rather than erroring — shadow mode only. Outside shadow
mode an unresolvable config fails the run.

Report encoding is done and tested in [`pkg/strategy`](../pkg/strategy):

```go
r := strategy.Report{ChainSelector: sel, Sequence: seq, ObservedAt: observedAt}
report, err := r.Build(strategy.ActionWithdrawShortfall)
```

There is no params argument, by design: `CREStrategyExecutor._processReport`
ignores params and recomputes every ETH amount from live state.

What issue #4 still owes: the on-chain reads (`lastSequence()`,
`strategyUpkeepStatus()`, Controller / StrategyManager / ExitQueue state) that
decide *which* action to propose, and the `writeReport` call itself.

## Rules to read first

[`docs/envelope.md`](../docs/envelope.md) — in particular:

- read `lastSequence()` every run; never keep a local counter
- propose actions in `strategy.Priority` order, matching
  `strategyUpkeepStatus`, so shadow-mode divergence monitoring
  ([issue #5](https://github.com/everstrat-xyz/keepers/issues/5)) does not
  report false positives
- `params` stay empty — an amount in a W2 report is a bug, not an optimisation
