/**
 * W1 queue-keeper decision engine — ported from the CRE-era Go implementation
 * (`pkg/queue/decide.go`) with its semantics intact.
 *
 * The engine decides what `QueueKeeperExecutor.perform` should be called with,
 * and the params surface here stays closed: the only expressible values are a
 * batch id and an index range. There is no path to put an ETH amount, NAV, or
 * price into a W1 payload, and `decodeParams` rejects any blob longer than the
 * action's exact encoding — which is what an appended amount word would look
 * like on the wire. The executor re-derives everything from live state anyway;
 * the discipline exists so a future edit cannot smuggle an authoritative value
 * into a payload and start trusting it.
 */

import { convertAssets, isRelativelyLessThan } from "./solmath";

/**
 * `QueueKeeperExecutor.MAX_BATCH_SCAN` — the gas bound on the on-chain
 * `queueUpkeepStatus` view and on `_advanceBatchCursor`.
 *
 * W1 deliberately scans past it (see decide). The executor's `_processReport`
 * does *not* apply the window when validating a `ProcessRequests` claim, so a
 * batch found beyond it is still executable — that is the whole point of
 * scanning off-chain. `AdvanceCursor` is the exception, and decide caps that
 * claim accordingly.
 */
export const MAX_BATCH_SCAN = 25n;

export enum Action {
  None = 0,
  PriceBatch = 1,
  ProcessRequests = 2,
  AdvanceCursor = 3,
}

export function actionString(a: Action): string {
  switch (a) {
    case Action.None:
      return "None";
    case Action.PriceBatch:
      return "PriceBatch";
    case Action.ProcessRequests:
      return "ProcessRequests";
    case Action.AdvanceCursor:
      return "AdvanceCursor";
    default:
      return `Action(${a as number})`;
  }
}

/** One queued redemption, as read from `ExitQueue.requestInfo`. */
export interface Request {
  user: string;
  processed: boolean;
  closedDueToSlippage: boolean;
  evePriceAtRequestTime: bigint;
  tokensToBurn: bigint;
  priceTolerance: bigint;
}

/** One ExitQueue batch: `batchInfo` plus the unprocessed user list. */
export interface Batch {
  id: bigint;
  canBeProcessed: boolean;
  finalEvePrice: bigint;
  totalTokensToBurn: bigint;
  createdAt: bigint;
  pricedAt: bigint;
  unprocessedCount: bigint;

  /**
   * Unprocessed requests in `unprocessedUsers` order, starting at index 0. The
   * executor only accepts a prefix of this list, so order is load-bearing, not
   * incidental. May be shorter than unprocessedCount when the read was capped
   * at maxUsersPerUpkeep — which is all the affordability model needs, since
   * the executor caps there too.
   */
  requests: Request[];
}

/** The full off-chain snapshot W1 decides from — one tick, no memory. */
export interface State {
  now: bigint;
  /** True when the executor or any of Controller / ExitQueue / AMM is paused. */
  paused: boolean;
  currentBatchId: bigint;
  /** The executor's stored cursor (`nextBatchIdToProcess`). */
  nextBatchIdToProcess: bigint;
  /** The ETH budget redemptions are paid from — the Controller's balance. */
  controllerBalance: bigint;
  maxBatchProcessingTime: bigint;
  minBatchAge: bigint;
  maxUsersPerUpkeep: bigint;
  /** Full scan, keyed by batch id as a decimal string. */
  batches: Record<string, Batch>;
  /**
   * Last batch id the read layer actually fetched when it capped the scan.
   * Zero means the scan reached the current batch. A truncated scan can only
   * cause W1 to propose *less* work than exists, never wrong work — but it
   * makes "found nothing" ambiguous, so decide refuses PriceBatch until a
   * later tick can finish the walk.
   */
  scanTruncatedAt: bigint;
}

export interface Decision {
  action: Action;
  batchId: bigint;
  /** Exclusive end of the claimed affordable prefix (ProcessRequests only). */
  endIndex: bigint;
  reason: string;
  /**
   * The chosen batch sits past MAX_BATCH_SCAN from the executor's stored
   * cursor — i.e. the on-chain view structurally could not have found it.
   * Divergence classification uses this to separate a real improvement from
   * a bug.
   */
  scannedBeyondWindow: boolean;
}

export function getBatch(s: State, id: bigint): Batch | undefined {
  return s.batches[id.toString()];
}

export function scanTruncated(s: State): boolean {
  return s.scanTruncatedAt !== 0n && s.scanTruncatedAt < s.currentBatchId;
}

/**
 * Mirrors `QueueKeeperExecutor._isBatchSkippable`.
 *
 * The unpriced guard comes first, so an unpriced batch — including the current
 * one, even when still empty — is never skippable: it can still receive
 * requests and must be priced first. A priced batch is skippable when fully
 * processed, or past MAX_BATCH_PROCESSING_TIME (the escape hatch, after which
 * users close their own requests and the keeper must not touch the batch).
 *
 * An unreadable batch is *not* skippable: skipping past a batch we could not
 * read would advance the cursor over live work.
 */
export function isBatchSkippable(s: State, id: bigint): boolean {
  const b = getBatch(s, id);
  if (!b) return false;
  if (!b.canBeProcessed) return false;
  if (b.unprocessedCount === 0n) return true;
  return s.now > b.pricedAt + s.maxBatchProcessingTime;
}

/**
 * Mirrors `QueueKeeperExecutor._affordableRequests`: how many requests, taken
 * as a prefix from index 0, the Controller's balance covers.
 *
 * The contract walks the prefix accumulating cost and stops at the first
 * request that would overrun the balance — it does not skip an expensive
 * request to fit cheaper ones behind it. Reproducing the break (rather than
 * "fit as many as possible") is what keeps the claim acceptable to the
 * executor.
 *
 * A batch past its processing window returns zero: the executor's own walk
 * returns zero there and the batch is the users' to close, not the keeper's.
 */
export function affordableRequests(s: State, id: bigint): bigint {
  const b = getBatch(s, id);
  if (!b) return 0n;
  if (!b.canBeProcessed) return 0n;
  if (b.pricedAt > 0n && s.now > b.pricedAt + s.maxBatchProcessingTime) return 0n;
  if (b.unprocessedCount === 0n) return 0n;

  let limit = b.unprocessedCount;
  if (limit > s.maxUsersPerUpkeep) limit = s.maxUsersPerUpkeep;
  if (BigInt(b.requests.length) < limit) limit = BigInt(b.requests.length);

  let cumulative = 0n;
  let count = 0n;
  for (let i = 0n; i < limit; i++) {
    const cost = requestCost(b.finalEvePrice, b.requests[Number(i)]);
    const next = cumulative + cost;
    if (next > s.controllerBalance) break;
    cumulative = next;
    count++;
  }
  return count;
}

/**
 * Mirrors the ETH a single request costs the Controller, per
 * `Controller._processRequest`. A request whose batch price fell more than the
 * user's tolerance below their queued price is closed at zero cost — it
 * consumes a slot but no ETH.
 */
export function requestCost(finalEvePrice: bigint, r: Request): bigint {
  if (isRelativelyLessThan(finalEvePrice, r.evePriceAtRequestTime, r.priceTolerance)) {
    return 0n;
  }
  return convertAssets(r.tokensToBurn, finalEvePrice);
}

/**
 * The first batch id `queueUpkeepStatus` cannot reach. The view walks its own
 * bounded cursor first, then scans at most MAX_BATCH_SCAN batches from wherever
 * that lands — so its reach is two windows deep from the stored cursor, not
 * one. Both decide and classify go through this helper so "did the view have a
 * chance to see this?" has exactly one answer.
 */
export function onChainScanWindowEnd(s: State): bigint {
  return peekAdvancedCursor(s, MAX_BATCH_SCAN) + MAX_BATCH_SCAN;
}

/**
 * Mirrors `QueueKeeperExecutor._peekAdvancedCursor`. scanLimit bounds how far
 * the cursor may walk: pass MAX_BATCH_SCAN to reproduce what the executor will
 * do; pass 0n for an unbounded off-chain walk.
 */
export function peekAdvancedCursor(s: State, scanLimit: bigint): bigint {
  let cursor = s.nextBatchIdToProcess;
  let stop = s.currentBatchId;
  if (scanLimit > 0n && cursor + scanLimit < stop) {
    stop = cursor + scanLimit;
  }
  while (cursor < stop && isBatchSkippable(s, cursor)) {
    cursor++;
  }
  return cursor;
}

/**
 * Decide selects the action W1 proposes, following the same priority order as
 * `QueueKeeperExecutor.queueUpkeepStatus` — process work first, then price the
 * current batch, then advance the cursor.
 *
 * The deliberate difference from the on-chain view is the scan width: this
 * walks every batch from the cursor to the current one, where the view stops
 * after MAX_BATCH_SCAN. That is the improvement the off-chain keeper exists to
 * provide, and it is safe for `ProcessRequests` because `_processReport`
 * re-derives affordability for the named batch without applying the window.
 *
 * `AdvanceCursor` gets the opposite treatment: the executor advances its
 * cursor with the *bounded* walk, so a claim past `cursor + MAX_BATCH_SCAN` is
 * unreachable in one execution and reverts. Decide therefore claims only the
 * bounded cursor.
 */
export function decide(s: State): Decision {
  if (s.paused) {
    return none("executor or a protocol contract is paused");
  }

  const fullCursor = peekAdvancedCursor(s, 0n);
  const windowEnd = onChainScanWindowEnd(s);

  // Process the oldest batch with an affordable prefix. Full scan.
  for (let id = fullCursor; id < s.currentBatchId; id++) {
    if (isBatchSkippable(s, id)) continue;
    const affordable = affordableRequests(s, id);
    if (affordable === 0n) continue;
    return {
      action: Action.ProcessRequests,
      batchId: id,
      endIndex: affordable,
      scannedBeyondWindow: id >= windowEnd,
      reason:
        `batch ${id} has ${affordable} affordable requests within a controller ` +
        `balance of ${s.controllerBalance} wei`,
    };
  }

  // PriceBatch is accepted even when ProcessRequests was also due, and would
  // grow the live-priced set instead of settling. If the process walk did not
  // finish, we cannot know that nothing was affordable — skip pricing this
  // tick. AdvanceCursor is still safe: it only walks skippable batches we
  // already have headers for.
  if (!scanTruncated(s)) {
    const b = getBatch(s, s.currentBatchId);
    if (b && b.unprocessedCount > 0n && s.now >= b.createdAt) {
      const age = s.now - b.createdAt;
      if (age >= s.minBatchAge) {
        return {
          action: Action.PriceBatch,
          batchId: s.currentBatchId,
          endIndex: 0n,
          scannedBeyondWindow: false,
          reason:
            `current batch ${s.currentBatchId} has ${b.unprocessedCount} unprocessed ` +
            `requests and is ${age}s old (minBatchAge ${s.minBatchAge}s)`,
        };
      }
    }
  }

  // Advance the cursor past dead batches — capped at what the executor can
  // actually reach in one execution.
  const boundedCursor = peekAdvancedCursor(s, MAX_BATCH_SCAN);
  if (boundedCursor > s.nextBatchIdToProcess) {
    return {
      action: Action.AdvanceCursor,
      batchId: boundedCursor,
      endIndex: 0n,
      scannedBeyondWindow: false,
      reason:
        `cursor can advance from ${s.nextBatchIdToProcess} to ${boundedCursor} ` +
        `past fully-processed or expired batches`,
    };
  }

  return none("no batch needs pricing, processing, or a cursor advance");
}

function none(reason: string): Decision {
  return { action: Action.None, batchId: 0n, endIndex: 0n, reason, scannedBeyondWindow: false };
}
