# Blueprints — how the keeper plane works

Mermaid diagrams explaining this repo. The per-workflow CRE-era diagrams were
removed with the Go workflows they described: W1 now lives as a Mimic function
(`mimic-functions/queue-keeper/`), W2 is a thin relay —
`StrategyKeeperExecutor.checker()` stays authoritative — and W4 is gone
entirely.

| Diagram | What it shows |
| --- | --- |
| `01-system-overview.md` | The components, Mimic, and the contracts, end to end |

Two invariants that explain half the design, worth keeping in mind while
reading all of these:

1. **Functions orchestrate, contracts decide.** Payloads carry *claims* (batch
   ids, actions) — never amounts. Executors re-derive everything from live
   state before acting.
2. **W1's `_execute(ProcessRequests)` has no scan window; W2's `_execute` uses
   the same bounded helpers as its view.** Equal 25-caps plus W1 also peeking
   +25 skippable on every `perform` mean the extra depth does not show up
   with this keeper as the only caller. A "truer" W2 number would revert.
