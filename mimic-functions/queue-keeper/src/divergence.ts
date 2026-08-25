/**
 * Divergence classification — W1's decision vs the on-chain `queueUpkeepStatus`
 * view, ported from the CRE-era Go implementation.
 *
 * Shadow mode's exit criterion is "zero unexplained divergences over 7 days",
 * which only means something if "explained" is defined in code rather than
 * judged per incident. These are those definitions.
 */

import { BigInt } from '@mimicprotocol/lib-ts'

import {
  ActionAdvanceCursor,
  ActionNone,
  ActionProcessRequests,
  actionString,
  Decision,
  MAX_BATCH_SCAN,
  onChainScanWindowEnd,
  peekAdvancedCursor,
  scanTruncated,
  State,
} from './decide'

/** The result of the on-chain `queueUpkeepStatus()` cross-check view. */
export class UpkeepStatus {
  /** Raw uint8 from the wire; compare with `Action.*` values. */
  action: u8
  batchId: BigInt
  count: BigInt

  constructor(action: u8, batchId: BigInt, count: BigInt) {
    this.action = action
    this.batchId = batchId
    this.count = count
  }
}

/** Classification labels, as plain strings so they serialize into logs. */
export const DivMatch: string = 'match'
/**
 * The run found work the gas-bounded view structurally could not see, or
 * claimed a valid shorter prefix. Expected — and the reason W1 exists.
 */
export const DivIntendedImprovement: string = 'intended-improvement'
/**
 * The scan cap stopped the queue scan short, so a missing ProcessRequests
 * (or a skipped PriceBatch) is the run refusing to guess, not a logic error.
 */
export const DivTruncatedScan: string = 'truncated-scan'
/**
 * Anything else. The run disagrees with the view in a way the scan window
 * does not explain, so either the off-chain model or the read layer is
 * wrong. This is what must stay at zero.
 */
export const DivBug: string = 'bug'

export class Divergence {
  klass: string
  decision: Decision
  onChain: UpkeepStatus
  explanation: string

  constructor(klass: string, decision: Decision, onChain: UpkeepStatus, explanation: string) {
    this.klass = klass
    this.decision = decision
    this.onChain = onChain
    this.explanation = explanation
  }
}

export function unexplained(d: Divergence): bool {
  return d.klass == DivBug
}

/** Compares a run decision against the on-chain view. */
export function classify(decision: Decision, onChain: UpkeepStatus, s: State): Divergence {
  const oc = onChain
  const boundedCursor = peekAdvancedCursor(s, MAX_BATCH_SCAN)
  const windowEnd = onChainScanWindowEnd(s)

  if (decision.action == ActionNone && oc.action == ActionNone) {
    return new Divergence(DivMatch, decision, oc, 'both agree there is no upkeep')
  }

  if (
    decision.action == ActionProcessRequests &&
    oc.action == ActionProcessRequests &&
    decision.batchId == oc.batchId
  ) {
    if (decision.endIndex == oc.count) {
      return new Divergence(DivMatch, decision, oc, 'same batch and same affordable prefix')
    }
    if (decision.endIndex < oc.count) {
      // The executor accepts any prefix, so a shorter claim is safe.
      return new Divergence(
        DivIntendedImprovement,
        decision,
        oc,
        'claiming a shorter prefix (' +
          decision.endIndex.toString() +
          ' of ' +
          oc.count.toString() +
          ' affordable) — accepted by the executor'
      )
    }
    return new Divergence(
      DivBug,
      decision,
      oc,
      'claiming ' +
        decision.endIndex.toString() +
        ' requests but the on-chain view finds only ' +
        oc.count.toString() +
        ' affordable — the executor would revert KeeperExecutorNoUpkeepNeeded'
    )
  }

  if (decision.action == ActionProcessRequests && decision.batchId >= windowEnd) {
    // The genuine full-scan win: this batch is past where the view stops.
    return new Divergence(
      DivIntendedImprovement,
      decision,
      oc,
      'batch ' +
        decision.batchId.toString() +
        ' is beyond the on-chain scan window (cursor ' +
        boundedCursor.toString() +
        ' + ' +
        MAX_BATCH_SCAN.toString() +
        ') — found by the off-chain full scan'
    )
  }

  if (decision.action == oc.action && decision.batchId == oc.batchId) {
    return new Divergence(DivMatch, decision, oc, 'same action and batch')
  }

  if (scanTruncated(s) && decision.action != oc.action) {
    return new Divergence(
      DivTruncatedScan,
      decision,
      oc,
      'scan cap truncated the queue scan at batch ' +
        s.scanTruncatedAt.toString() +
        '; run proposes ' +
        actionString(decision.action) +
        ', view recommends ' +
        actionString(oc.action) +
        ' on batch ' +
        oc.batchId.toString()
    )
  }

  if (decision.action == ActionNone && oc.action != ActionNone) {
    return new Divergence(
      DivBug,
      decision,
      oc,
      'on-chain view recommends ' +
        actionString(oc.action) +
        ' on batch ' +
        oc.batchId.toString() +
        ' but the run proposes nothing — upkeep would stall'
    )
  }

  if (decision.action == ActionAdvanceCursor && decision.batchId > windowEnd) {
    // Decide caps AdvanceCursor at the bounded cursor, so this means the cap
    // was bypassed and the executor could not reach the claim.
    return new Divergence(
      DivBug,
      decision,
      oc,
      'cursor claim ' +
        decision.batchId.toString() +
        ' exceeds what the executor can reach in one ' +
        'execution (bounded cursor ' +
        boundedCursor.toString() +
        ')'
    )
  }

  return new Divergence(
    DivBug,
    decision,
    oc,
    'run proposes ' +
      actionString(decision.action) +
      ' on batch ' +
      decision.batchId.toString() +
      ', on-chain view recommends ' +
      actionString(oc.action) +
      ' on batch ' +
      oc.batchId.toString() +
      ', and the scan window does not explain the difference'
  )
}
