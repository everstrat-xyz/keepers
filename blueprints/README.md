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
2. **W1 may scan deeper than the contract; W2 may not.** W1's executor
   validates `ProcessRequests` per batch with no scan window, so scanning deep
   off-chain genuinely wins. W2's executor re-derives quantities with the same
   bounded helpers the view uses, so a "truer" number would revert every time.
