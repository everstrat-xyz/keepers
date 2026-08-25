import { Action, decide } from "./decide";
import { DivergenceClass, classify, unexplained } from "./divergence";
import { batch, done, req, stateWith } from "./fixtures";
import type { Decision, State } from "./decide";
import type { UpkeepStatus } from "./divergence";

// A plain state with one processable batch at the cursor — the on-chain view
// sees everything here, so any disagreement is a bug.
const plain = stateWith([batch(1, req(1, 1), req(2, 1))]);

const d = (action: Action, batchId = 0n, endIndex = 0n): Decision => ({
  action,
  batchId,
  endIndex,
  reason: "test",
  scannedBeyondWindow: false,
});

const oc = (action: Action, batchId = 0n, count = 0n): UpkeepStatus => ({ action, batchId, count });

describe("classify", () => {
  it.each([
    ["both idle", d(Action.None), oc(Action.None), plain, DivergenceClass.Match],
    ["same batch and prefix", d(Action.ProcessRequests, 1n, 2n), oc(Action.ProcessRequests, 1n, 2n), plain, DivergenceClass.Match],
    ["same PriceBatch", d(Action.PriceBatch, 1n), oc(Action.PriceBatch, 1n), plain, DivergenceClass.Match],
    // The executor accepts any prefix of the affordable set, so claiming
    // fewer is safe.
    ["shorter prefix claimed", d(Action.ProcessRequests, 1n, 1n), oc(Action.ProcessRequests, 1n, 2n), plain, DivergenceClass.IntendedImprovement],
    // Over-claiming reverts KeeperExecutorNoUpkeepNeeded on-chain.
    ["longer prefix claimed", d(Action.ProcessRequests, 1n, 5n), oc(Action.ProcessRequests, 1n, 2n), plain, DivergenceClass.Bug],
    ["run idle while the view has work", d(Action.None), oc(Action.ProcessRequests, 1n, 2n), plain, DivergenceClass.Bug],
    ["different batches for the same action", d(Action.ProcessRequests, 2n, 1n), oc(Action.ProcessRequests, 1n, 2n), plain, DivergenceClass.Bug],
    ["different actions entirely", d(Action.PriceBatch, 1n), oc(Action.AdvanceCursor, 3n), plain, DivergenceClass.Bug],
  ] as const)("%s", (_name, decision, onChain, state, want) => {
    const got = classify(decision, onChain, state);
    expect(got.class).toBe(want);
    expect(got.explanation.length).toBeGreaterThan(0); // triage depends on it
  });

  it("a truncated scan explains idle-vs-view-work", () => {
    const s = { ...plain, scanTruncatedAt: 1n };
    const got = classify(d(Action.None), oc(Action.ProcessRequests, 1n, 2n), s);
    expect(got.class).toBe(DivergenceClass.TruncatedScan);
  });

  it("a truncated scan explains a skipped PriceBatch", () => {
    const s = { ...plain, currentBatchId: 2n, scanTruncatedAt: 1n };
    const got = classify(d(Action.None), oc(Action.PriceBatch, 2n), s);
    expect(got.class).toBe(DivergenceClass.TruncatedScan);
  });

  // The divergence that must NOT count against the shadow window: the
  // off-chain full scan finds a processable batch past where the view stops
  // looking.
  it("beyond-window work is an improvement, not a bug", () => {
    const batches = Array.from({ length: 60 }, (_, i) => done(i + 1));
    batches.push(batch(61, req(9, 1)));
    const s = stateWith(batches, { currentBatchId: 62n });

    const decision = decide(s);
    // The gas-bounded view walks to 26, scans [26,51), finds only processed
    // batches, and reports None.
    const got = classify(decision, oc(Action.None), s);

    expect(got.class).toBe(DivergenceClass.IntendedImprovement);
    expect(unexplained(got)).toBe(false);
    expect(got.explanation).toContain("beyond the on-chain scan window");
  });
});

describe("unexplained", () => {
  it.each([
    [DivergenceClass.Match, false],
    [DivergenceClass.IntendedImprovement, false],
    [DivergenceClass.TruncatedScan, false],
    [DivergenceClass.Bug, true],
  ])("%s → %s", (cls, want) => {
    expect(unexplained({ class: cls, decision: d(Action.None), onChain: oc(Action.None), explanation: "" })).toBe(want);
  });
});
