/**
 * Shared fixtures — the same values the Go suite used, so a behavioral
 * difference between the Go and TS decision engines shows up as a failing
 * assertion rather than a plausible-looking refactor.
 */

import { Batch, Request, State } from "./decide";

export const ONE_ETH = 10n ** 18n;
export const NOW_TS = 1_700_000_000n;
export const DAY_S = 86_400n;
export const THREE_DAYS_S = 3n * DAY_S;

export function eth(n: bigint | number): bigint {
  return BigInt(n) * ONE_ETH;
}

export function user(n: number): string {
  // Address with only the last byte set — unique, and readable in output.
  return "0x" + "00".repeat(19) + n.toString(16).padStart(2, "0");
}

/** A request costing exactly `tokens` ETH at 1 ETH per EVE, no slippage closure. */
export function req(n: number, tokens: bigint | number): Request {
  return {
    user: user(n),
    processed: false,
    closedDueToSlippage: false,
    evePriceAtRequestTime: eth(1),
    tokensToBurn: eth(tokens),
    priceTolerance: 0n,
  };
}

/** A priced, processable batch from a request list. */
export function batch(id: bigint | number, ...reqs: Request[]): Batch {
  return {
    id: BigInt(id),
    canBeProcessed: true,
    finalEvePrice: eth(1),
    totalTokensToBurn: reqs.reduce((sum, r) => sum + r.tokensToBurn, 0n),
    createdAt: NOW_TS - 2n * DAY_S,
    pricedAt: NOW_TS - DAY_S,
    unprocessedCount: BigInt(reqs.length),
    requests: reqs,
  };
}

export function stateWith(batches: Batch[], overrides: Partial<State> = {}): State {
  const map: Record<string, Batch> = {};
  let maxId = 0n;
  for (const b of batches) {
    map[b.id.toString()] = b;
    if (b.id > maxId) maxId = b.id;
  }
  return {
    now: NOW_TS,
    paused: false,
    currentBatchId: maxId + 1n,
    nextBatchIdToProcess: 1n,
    controllerBalance: eth(1000),
    maxBatchProcessingTime: THREE_DAYS_S,
    minBatchAge: DAY_S,
    maxUsersPerUpkeep: 20n,
    batches: map,
    scanTruncatedAt: 0n,
    ...overrides,
  };
}

/** A fully-processed batch the cursor can walk past. */
export function done(id: bigint | number): Batch {
  const b = batch(id, req(1, 1));
  b.unprocessedCount = 0n;
  b.requests = [];
  return b;
}

/** The current batch: unpriced, receiving requests. */
export function current(createdSecondsAgo = 2n * DAY_S): Batch {
  return {
    id: 0n, // caller sets via stateWith's maxId+1
    canBeProcessed: false,
    finalEvePrice: 0n,
    totalTokensToBurn: 0n,
    createdAt: NOW_TS - createdSecondsAgo,
    pricedAt: 0n,
    unprocessedCount: 0n,
    requests: [],
  };
}
