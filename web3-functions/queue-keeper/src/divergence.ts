/**
 * Divergence classification — W1's decision vs the on-chain `queueUpkeepStatus`
 * view, ported from the CRE-era Go implementation.
 *
 * Shadow mode's exit criterion is "zero unexplained divergences over 7 days",
 * which only means something if "explained" is defined in code rather than
 * judged per incident. These are those definitions.
 */

import { Action, Decision, State, MAX_BATCH_SCAN, onChainScanWindowEnd, peekAdvancedCursor, scanTruncated } from "./decide";

/** The result of the on-chain `queueUpkeepStatus()` cross-check view. */
export interface UpkeepStatus {
  /** Accepts the raw uint8 the wire returns or a typed Action. */
  action: Action | number;
  batchId: bigint;
  count: bigint;
}

export enum DivergenceClass {
  /** The run and the on-chain view agree. */
  Match = "match",
  /**
   * The run found work the gas-bounded view structurally could not see, or
   * claimed a valid shorter prefix. Expected — and the reason W1 exists.
   */
  IntendedImprovement = "intended-improvement",
  /**
   * The scan cap stopped the queue scan short, so a missing ProcessRequests
   * (or a skipped PriceBatch) is the run refusing to guess, not a logic error.
   */
  TruncatedScan = "truncated-scan",
  /**
   * Anything else. The run disagrees with the view in a way the scan window
   * does not explain, so either the off-chain model or the read layer is
   * wrong. This is what must stay at zero.
   */
  Bug = "bug",
}

export interface Divergence {
  class: DivergenceClass;
  decision: Decision;
  onChain: UpkeepStatus;
  explanation: string;
}

export function unexplained(d: Divergence): boolean {
  return d.class === DivergenceClass.Bug;
}

/** Compares a run decision against the on-chain view. */
export function classify(decision: Decision, onChain: UpkeepStatus, s: State): Divergence {
  // Normalize once: the wire returns a raw uint8, tests pass a typed Action.
  const oc = { ...onChain, action: onChain.action as number };
  const boundedCursor = peekAdvancedCursor(s, MAX_BATCH_SCAN);
  const windowEnd = onChainScanWindowEnd(s);

  const d: Divergence = { class: DivergenceClass.Bug, decision, onChain, explanation: "" };

  if (decision.action === Action.None && oc.action === Action.None) {
    d.class = DivergenceClass.Match;
    d.explanation = "both agree there is no upkeep";
    return d;
  }

  if (
    decision.action === Action.ProcessRequests &&
    oc.action === Action.ProcessRequests &&
    decision.batchId === oc.batchId
  ) {
    if (decision.endIndex === oc.count) {
      d.class = DivergenceClass.Match;
      d.explanation = "same batch and same affordable prefix";
    } else if (decision.endIndex < oc.count) {
      // The executor accepts any prefix, so a shorter claim is safe.
      d.class = DivergenceClass.IntendedImprovement;
      d.explanation =
        `claiming a shorter prefix (${decision.endIndex} of ${oc.count} affordable) ` +
        `— accepted by the executor`;
    } else {
      d.explanation =
        `claiming ${decision.endIndex} requests but the on-chain view finds only ` +
        `${oc.count} affordable — the executor would revert KeeperExecutorNoUpkeepNeeded`;
    }
    return d;
  }

  if (decision.action === Action.ProcessRequests && decision.batchId >= windowEnd) {
    // The genuine full-scan win: this batch is past where the view stops.
    d.class = DivergenceClass.IntendedImprovement;
    d.explanation =
      `batch ${decision.batchId} is beyond the on-chain scan window ` +
      `(cursor ${boundedCursor} + ${MAX_BATCH_SCAN}) — found by the off-chain full scan`;
    return d;
  }

  if (decision.action === oc.action && decision.batchId === oc.batchId) {
    d.class = DivergenceClass.Match;
    d.explanation = "same action and batch";
    return d;
  }

  if (scanTruncated(s) && decision.action !== oc.action) {
    d.class = DivergenceClass.TruncatedScan;
    d.explanation =
      `scan cap truncated the queue scan at batch ${s.scanTruncatedAt}; run proposes ` +
      `${decision.action}, view recommends ${oc.action} on batch ${oc.batchId}`;
    return d;
  }

  if (decision.action === Action.None && oc.action !== Action.None) {
    d.explanation =
      `on-chain view recommends ${oc.action} on batch ${oc.batchId} but the ` +
      `run proposes nothing — upkeep would stall`;
    return d;
  }

  if (decision.action === Action.AdvanceCursor && decision.batchId > windowEnd) {
    // Decide caps AdvanceCursor at the bounded cursor, so this means the cap
    // was bypassed and the executor could not reach the claim.
    d.explanation =
      `cursor claim ${decision.batchId} exceeds what the executor can reach in one ` +
      `execution (bounded cursor ${boundedCursor})`;
    return d;
  }

  d.explanation =
    `run proposes ${decision.action} on batch ${decision.batchId}, on-chain view recommends ` +
    `${oc.action} on batch ${oc.batchId}, and the scan window does not explain the difference`;
  return d;
}
