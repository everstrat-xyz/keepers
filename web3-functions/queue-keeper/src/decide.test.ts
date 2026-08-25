import {
  Action,
  MAX_BATCH_SCAN,
  affordableRequests,
  decide,
  isBatchSkippable,
  onChainScanWindowEnd,
  peekAdvancedCursor,
} from "./decide";
import { DAY_S, THREE_DAYS_S, batch, done, eth, req, stateWith } from "./fixtures";

describe("isBatchSkippable", () => {
  it.each([
    ["fully processed batch is skippable", (b: typeof batch1) => { b.unprocessedCount = 0n; }, true],
    ["priced batch with work left is not skippable", () => {}, false],
    ["unpriced batch is not skippable", (b: typeof batch1) => { b.pricedAt = 0n; b.canBeProcessed = false; }, false],
    // canBeProcessed false short-circuits before the expiry check.
    [
      "not-yet-processable batch is not skippable even when old",
      (b: typeof batch1) => { b.canBeProcessed = false; b.pricedAt = 1_700_000_000n - 10n * DAY_S; },
      false,
    ],
    // Contracts PR #43 reordered the guard: an unpriced batch is never
    // skippable even when empty, because it can still receive requests and
    // must be priced first.
    [
      "unpriced empty batch is not skippable",
      (b: typeof batch1) => { b.canBeProcessed = false; b.pricedAt = 0n; b.unprocessedCount = 0n; },
      false,
    ],
    ["expired batch past MAX_BATCH_PROCESSING_TIME is skippable", (b: typeof batch1) => { b.pricedAt = 1_700_000_000n - THREE_DAYS_S - 1n; }, true],
    // The contract uses a strict `>`, so exactly at the boundary the batch is
    // still live.
    ["batch exactly at the expiry boundary is not skippable", (b: typeof batch1) => { b.pricedAt = 1_700_000_000n - THREE_DAYS_S; }, false],
  ] as const)("%s", (_name, mutate, want) => {
    const b = batch(1, req(1, 1));
    mutate(b);
    expect(isBatchSkippable(stateWith([b]), 1n)).toBe(want);
  });

  it("unreadable batch is not skippable", () => {
    const s = stateWith([batch(1, req(1, 1))]);
    expect(isBatchSkippable(s, 99n)).toBe(false);
  });
});

const batch1 = batch(1, req(1, 1));

describe("affordableRequests", () => {
  it.each([
    ["all requests fit", [req(1, 1), req(2, 1), req(3, 1)], eth(10), 20n, 3n],
    ["balance covers a prefix only", [req(1, 1), req(2, 1), req(3, 1)], eth(2), 20n, 2n],
    // The contract breaks at the first unaffordable request rather than
    // skipping it to fit cheaper ones behind it.
    ["stops at the first unaffordable request", [req(1, 1), req(2, 100), req(3, 1)], eth(5), 20n, 1n],
    ["exact balance fits the whole prefix", [req(1, 1), req(2, 1)], eth(2), 20n, 2n],
    ["one wei short drops the last request", [req(1, 1), req(2, 1)], eth(2) - 1n, 20n, 1n],
    ["zero balance affords nothing", [req(1, 1)], 0n, 20n, 0n],
    ["capped at maxUsersPerUpkeep", [req(1, 1), req(2, 1), req(3, 1), req(4, 1)], eth(1000), 2n, 2n],
  ] as const)("%s", (_name, reqs, balance, maxUsers, want) => {
    const s = stateWith([batch(1, ...reqs)], { controllerBalance: balance, maxUsersPerUpkeep: maxUsers });
    expect(affordableRequests(s, 1n)).toBe(want);
  });

  it("unpriced batch affords nothing", () => {
    const b = batch(1, req(1, 1));
    b.canBeProcessed = false;
    expect(affordableRequests(stateWith([b]), 1n)).toBe(0n);
  });

  it("empty batch affords nothing", () => {
    const b = batch(1);
    b.unprocessedCount = 0n;
    expect(affordableRequests(stateWith([b]), 1n)).toBe(0n);
  });

  // Contracts PR #43: a batch past its window returns zero on both sides —
  // pullRequest reverts ExitQueueBatchExpired, so no balance settles it.
  it("expired batch affords nothing regardless of balance", () => {
    const b = batch(1, req(1, 1));
    b.pricedAt = 1_700_000_000n - THREE_DAYS_S - 1n;
    expect(affordableRequests(stateWith([b]), 1n)).toBe(0n);
  });

  // The slipped request costs nothing and must not stop the walk.
  it("a slippage-closed request consumes a slot but no budget", () => {
    const slipped = { ...req(2, 1000), priceTolerance: 50_000_000_000_000_000n };
    const b = batch(1, req(1, 1), slipped, req(3, 1));
    b.finalEvePrice = 900_000_000_000_000_000n; // 10% below, past the 5% tolerance
    const s = stateWith([b], { controllerBalance: eth(3) });
    expect(affordableRequests(s, 1n)).toBe(3n);
  });
});

describe("peekAdvancedCursor", () => {
  it("walks past processed batches", () => {
    const s = stateWith([done(1), done(2), batch(3, req(1, 1))]);
    expect(peekAdvancedCursor(s, 0n)).toBe(3n);
  });

  it("stops at live work", () => {
    const s = stateWith([done(1), batch(2, req(1, 1)), done(3)]);
    expect(peekAdvancedCursor(s, 0n)).toBe(2n);
  });

  it("never passes the current batch", () => {
    const s = stateWith([done(1), done(2)]);
    // CurrentBatchID is 3; the cursor must stop there.
    expect(peekAdvancedCursor(s, 0n)).toBe(3n);
  });

  // The bounded walk is what the executor performs, so it bounds any
  // AdvanceCursor claim.
  it("bounded walk stops after MAX_BATCH_SCAN", () => {
    const batches = Array.from({ length: 60 }, (_, i) => done(i + 1));
    batches.push(batch(61, req(1, 1)));
    const s = stateWith(batches);

    expect(peekAdvancedCursor(s, MAX_BATCH_SCAN)).toBe(1n + MAX_BATCH_SCAN);
    expect(peekAdvancedCursor(s, 0n)).toBe(61n);
  });
});

describe("decide", () => {
  it("paused short-circuits everything", () => {
    const s = stateWith([batch(1, req(1, 1))], { paused: true });
    expect(decide(s).action).toBe(Action.None);
  });

  it("processing beats pricing", () => {
    const cur = batch(2, req(5, 1));
    cur.canBeProcessed = false;
    cur.pricedAt = 0n;
    cur.createdAt = 1_700_000_000n - 2n * DAY_S;

    const s = stateWith([batch(1, req(1, 1)), cur], { currentBatchId: 2n });
    const d = decide(s);
    expect([d.action, d.batchId, d.endIndex]).toEqual([Action.ProcessRequests, 1n, 1n]);
  });

  it("prices the current batch when nothing is processable", () => {
    const cur = batch(1, req(1, 1));
    cur.canBeProcessed = false;
    cur.pricedAt = 0n;
    cur.createdAt = 1_700_000_000n - 2n * DAY_S;

    const s = stateWith([cur], { currentBatchId: 1n });
    const d = decide(s);
    expect([d.action, d.batchId]).toEqual([Action.PriceBatch, 1n]);
  });

  it("refuses PriceBatch when the process walk was truncated", () => {
    // Batch 1 may have work but its users were not loaded. Pricing batch 2
    // would be accepted on-chain even if batch 1 was processable — growing the
    // live-priced set instead of settling it.
    const head = batch(1, req(1, 1));
    head.requests = [];
    const cur = batch(2, req(2, 1));
    cur.canBeProcessed = false;
    cur.pricedAt = 0n;
    cur.createdAt = 1_700_000_000n - 2n * DAY_S;

    const s = stateWith([head, cur], { currentBatchId: 2n, scanTruncatedAt: 1n });
    const d = decide(s);
    expect(d.action).toBe(Action.None); // not PriceBatch, and AdvanceCursor is not due
  });

  it("still processes when truncated if an affordable prefix was loaded", () => {
    const s = stateWith([batch(1, req(1, 1))], { scanTruncatedAt: 1n });
    const d = decide(s);
    expect([d.action, d.batchId]).toEqual([Action.ProcessRequests, 1n]);
  });

  it("still advances the cursor when truncated", () => {
    const cur = batch(2);
    cur.unprocessedCount = 0n;
    cur.canBeProcessed = false;
    cur.createdAt = 1_700_000_000n;

    const s = stateWith([done(1), cur], { currentBatchId: 2n, scanTruncatedAt: 1n });
    const d = decide(s);
    expect([d.action, d.batchId]).toEqual([Action.AdvanceCursor, 2n]);
  });

  it("does not price a batch younger than minBatchAge", () => {
    const cur = batch(1, req(1, 1));
    cur.canBeProcessed = false;
    cur.pricedAt = 0n;
    cur.createdAt = 1_700_000_000n - DAY_S + 1n; // one second short

    const s = stateWith([cur], { currentBatchId: 1n });
    expect(decide(s).action).toBe(Action.None);
  });

  it("advances the cursor when there is nothing else", () => {
    const empty = batch(2);
    empty.unprocessedCount = 0n;
    empty.canBeProcessed = false;
    empty.createdAt = 1_700_000_000n;

    const s = stateWith([done(1), empty], { currentBatchId: 2n });
    const d = decide(s);
    expect([d.action, d.batchId]).toEqual([Action.AdvanceCursor, 2n]);
  });

  it("processes a later batch when the head is unaffordable", () => {
    // Mirrors queueUpkeepStatus: an in-window batch whose first request
    // overruns is not skippable, but affordable == 0, so the walk continues.
    const s = stateWith([batch(1, req(1, 100)), batch(2, req(2, 1))], { controllerBalance: eth(1) });
    const d = decide(s);
    expect([d.action, d.batchId, d.endIndex]).toEqual([Action.ProcessRequests, 2n, 1n]);
  });

  it("processes a later batch when the head is expired", () => {
    const head = batch(1, req(1, 1));
    head.pricedAt = 1_700_000_000n - THREE_DAYS_S - 1n;
    const s = stateWith([head, batch(2, req(2, 1))]);
    const d = decide(s);
    expect([d.action, d.batchId]).toEqual([Action.ProcessRequests, 2n]);
  });

  it("nothing to do", () => {
    const empty = batch(1);
    empty.unprocessedCount = 0n;
    empty.canBeProcessed = false;
    empty.createdAt = 1_700_000_000n;

    const s = stateWith([empty], { currentBatchId: 1n, nextBatchIdToProcess: 1n });
    expect(decide(s).action).toBe(Action.None);
  });
});

// The full-scan improvement W1 exists to deliver: a processable batch past
// MAX_BATCH_SCAN that queueUpkeepStatus structurally cannot reach.
describe("full-scan improvement", () => {
  it("finds work beyond the on-chain scan window", () => {
    // The view's reach is two windows deep: it walks its bounded cursor to
    // 1+25=26, then scans [26, 51). The target sits at 51+ to be invisible.
    const batches = Array.from({ length: 60 }, (_, i) => done(i + 1));
    batches.push(batch(61, req(9, 1)));

    const s = stateWith(batches, { currentBatchId: 62n });
    expect(onChainScanWindowEnd(s)).toBe(51n);

    const d = decide(s);
    expect([d.action, d.batchId]).toEqual([Action.ProcessRequests, 61n]);
    expect(d.scannedBeyondWindow).toBe(true);
  });

  // The other side of the boundary: a batch the view *can* reach must not be
  // labelled a full-scan win, or the shadow window fills with false
  // improvements hiding real bugs.
  it("does not overclaim an improvement inside the window", () => {
    const batches = Array.from({ length: 40 }, (_, i) => done(i + 1));
    batches.push(batch(41, req(9, 1)));

    const s = stateWith(batches, { currentBatchId: 42n });
    const d = decide(s);
    expect(d.batchId).toBe(41n);
    expect(d.scannedBeyondWindow).toBe(false);
  });
});

// Keeps the full scan from producing a revert storm: the executor advances the
// cursor with the *bounded* walk and reverts unless it reaches the claim.
it("caps AdvanceCursor at the executor's reach", () => {
  const batches = Array.from({ length: 60 }, (_, i) => done(i + 1));
  const cur = batch(61);
  cur.unprocessedCount = 0n;
  cur.canBeProcessed = false;
  cur.createdAt = 1_700_000_000n;
  batches.push(cur);

  const s = stateWith(batches, { currentBatchId: 61n });
  const d = decide(s);
  expect(d.action).toBe(Action.AdvanceCursor);
  expect(d.batchId).toBe(1n + MAX_BATCH_SCAN);
});
