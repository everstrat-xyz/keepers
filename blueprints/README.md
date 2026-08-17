# Blueprints — how the keeper plane works

Mermaid diagrams explaining this repo. Start with `01-system-overview`, then
read the workflow you care about. Every diagram mirrors the code and cites the
files it summarizes; when code and diagram disagree, the code wins — please
update the diagram in the same PR.

| Diagram | What it shows |
| --- | --- |
| `01-system-overview.md` | The three workflows, the DON, and the contracts, end to end |
| `02-w1-queue-keeper.md` | W1's tick: read plan, decision tree, report delivery |
| `03-w2-strategy-keeper.md` | W2's tick: read plan, decision tree, the bounded-scan rule |
| `04-w4-freeze-watch.md` | W4's tick: what it watches, alert kinds, webhook |
| `05-read-budget.md` | The 15-read constraint and how Multicall3 + Budget stretch it |
| `06-report-lifecycle.md` | Envelope layout, validation guards, DON signing, receiver handling |

Two invariants that explain half the design, worth keeping in mind while
reading all of these:

1. **Workflows orchestrate, contracts decide.** Reports carry *claims* (batch
   ids, actions) — never amounts. Receivers re-derive everything from live
   state before acting (`TECH_SPEC.md` §5).
2. **W1 may scan deeper than the contract; W2 may not.** W1's receiver
   validates `ProcessRequests` per batch with no scan window, so scanning deep
   off-chain genuinely wins. W2's receiver re-derives quantities with the same
   bounded helpers the view uses, so a "truer" number would revert every time.
