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

import { BigInt } from '@mimicprotocol/lib-ts'

import { convertAssets, isRelativelyLessThan } from './solmath'

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
export const MAX_BATCH_SCAN: BigInt = BigInt.fromI32(25)

/** QueueAction values from `IQueueKeeperExecutor`. */
export const ActionNone: u8 = 0
export const ActionPriceBatch: u8 = 1
export const ActionProcessRequests: u8 = 2
export const ActionAdvanceCursor: u8 = 3

export function actionString(a: u8): string {
  if (a == ActionNone) return 'None'
  if (a == ActionPriceBatch) return 'PriceBatch'
  if (a == ActionProcessRequests) return 'ProcessRequests'
  if (a == ActionAdvanceCursor) return 'AdvanceCursor'
  return 'Action(' + a.toString() + ')'
}

/** One queued redemption, as read from `ExitQueue.requestInfo`. */
export class Request {
  user: string
  processed: bool
  closedDueToSlippage: bool
  evePriceAtRequestTime: BigInt
  tokensToBurn: BigInt
  priceTolerance: BigInt

  constructor(
    user: string,
    processed: bool,
    closedDueToSlippage: bool,
    evePriceAtRequestTime: BigInt,
    tokensToBurn: BigInt,
    priceTolerance: BigInt
  ) {
    this.user = user
    this.processed = processed
    this.closedDueToSlippage = closedDueToSlippage
    this.evePriceAtRequestTime = evePriceAtRequestTime
    this.tokensToBurn = tokensToBurn
    this.priceTolerance = priceTolerance
  }
}

/** One ExitQueue batch: `batchInfo` plus the unprocessed user list. */
export class Batch {
  id: BigInt
  canBeProcessed: bool
  finalEvePrice: BigInt
  totalTokensToBurn: BigInt
  createdAt: BigInt
  pricedAt: BigInt
  unprocessedCount: BigInt

  /**
   * Unprocessed requests in `unprocessedUsers` order, starting at index 0. The
   * executor only accepts a prefix of this list, so order is load-bearing, not
   * incidental. May be shorter than unprocessedCount when the read was capped
   * at maxUsersPerUpkeep — which is all the affordability model needs, since
   * the executor caps there too.
   */
  requests: Request[]

  constructor(
    id: BigInt,
    canBeProcessed: bool,
    finalEvePrice: BigInt,
    totalTokensToBurn: BigInt,
    createdAt: BigInt,
    pricedAt: BigInt,
    unprocessedCount: BigInt,
    requests: Request[]
  ) {
    this.id = id
    this.canBeProcessed = canBeProcessed
    this.finalEvePrice = finalEvePrice
    this.totalTokensToBurn = totalTokensToBurn
    this.createdAt = createdAt
    this.pricedAt = pricedAt
    this.unprocessedCount = unprocessedCount
    this.requests = requests
  }
}

/** The full off-chain snapshot W1 decides from — one tick, no memory. */
export class State {
  now: BigInt
  /** True when the executor or any of Controller / ExitQueue / AMM is paused. */
  paused: bool
  currentBatchId: BigInt
  /** The executor's stored cursor (`nextBatchIdToProcess`). */
  nextBatchIdToProcess: BigInt
  /** The ETH budget redemptions are paid from — the Controller's balance. */
  controllerBalance: BigInt
  maxBatchProcessingTime: BigInt
  minBatchAge: BigInt
  maxUsersPerUpkeep: BigInt
  /** Full scan, keyed by batch id as a decimal string. */
  batches: Map<string, Batch>
  /**
   * Last batch id the read layer actually fetched when it capped the scan.
   * Zero means the scan reached the current batch. A truncated scan can only
   * cause W1 to propose *less* work than exists, never wrong work — but it
   * makes "found nothing" ambiguous, so decide refuses PriceBatch until a
   * later tick can finish the walk.
   */
  scanTruncatedAt: BigInt

  constructor(
    now: BigInt,
    paused: bool,
    currentBatchId: BigInt,
    nextBatchIdToProcess: BigInt,
    controllerBalance: BigInt,
    maxBatchProcessingTime: BigInt,
    minBatchAge: BigInt,
    maxUsersPerUpkeep: BigInt,
    batches: Map<string, Batch>,
    scanTruncatedAt: BigInt
  ) {
    this.now = now
    this.paused = paused
    this.currentBatchId = currentBatchId
    this.nextBatchIdToProcess = nextBatchIdToProcess
    this.controllerBalance = controllerBalance
    this.maxBatchProcessingTime = maxBatchProcessingTime
    this.minBatchAge = minBatchAge
    this.maxUsersPerUpkeep = maxUsersPerUpkeep
    this.batches = batches
    this.scanTruncatedAt = scanTruncatedAt
  }
}

export class Decision {
  action: u8
  batchId: BigInt
  /** Exclusive end of the claimed affordable prefix (ProcessRequests only). */
  endIndex: BigInt
  reason: string
  /**
   * The chosen batch sits past MAX_BATCH_SCAN from the executor's stored
   * cursor — i.e. the on-chain view structurally could not have found it.
   * Divergence classification uses this to separate a real improvement from
   * a bug.
   */
  scannedBeyondWindow: bool

  constructor(action: u8, batchId: BigInt, endIndex: BigInt, reason: string, scannedBeyondWindow: bool) {
    this.action = action
    this.batchId = batchId
    this.endIndex = endIndex
    this.reason = reason
    this.scannedBeyondWindow = scannedBeyondWindow
  }
}

export function getBatch(s: State, id: BigInt): Batch | null {
  // AssemblyScript's Map.get aborts the whole module on a missing key, and a
  // truncated scan leaves ids past scanEnd absent — so probe membership first.
  const key = id.toString()
  if (!s.batches.has(key)) return null
  return s.batches.get(key)
}

export function scanTruncated(s: State): bool {
  return s.scanTruncatedAt != BigInt.zero() && s.scanTruncatedAt < s.currentBatchId
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
export function isBatchSkippable(s: State, id: BigInt): bool {
  const b = getBatch(s, id)
  if (b == null) return false
  if (!b.canBeProcessed) return false
  if (b.unprocessedCount == BigInt.zero()) return true
  return s.now > b.pricedAt.plus(s.maxBatchProcessingTime)
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
export function affordableRequests(s: State, id: BigInt): BigInt {
  const b = getBatch(s, id)
  if (b == null) return BigInt.zero()
  if (!b.canBeProcessed) return BigInt.zero()
  if (b.pricedAt > BigInt.zero() && s.now > b.pricedAt.plus(s.maxBatchProcessingTime)) return BigInt.zero()
  if (b.unprocessedCount == BigInt.zero()) return BigInt.zero()

  let limit = b.unprocessedCount
  if (limit > s.maxUsersPerUpkeep) limit = s.maxUsersPerUpkeep
  if (BigInt.fromI32(b.requests.length) < limit) limit = BigInt.fromI32(b.requests.length)

  let cumulative = BigInt.zero()
  let count = BigInt.zero()
  for (let i = BigInt.zero(); i < limit; i = i.plus(BigInt.fromI32(1))) {
    const cost = requestCost(b.finalEvePrice, b.requests[i.toI32()])
    const next = cumulative.plus(cost)
    if (next > s.controllerBalance) break
    cumulative = next
    count = count.plus(BigInt.fromI32(1))
  }
  return count
}

/**
 * Mirrors the ETH a single request costs the Controller, per
 * `Controller._processRequest`. A request whose batch price fell more than the
 * user's tolerance below their queued price is closed at zero cost — it
 * consumes a slot but no ETH.
 */
export function requestCost(finalEvePrice: BigInt, r: Request): BigInt {
  if (isRelativelyLessThan(finalEvePrice, r.evePriceAtRequestTime, r.priceTolerance)) {
    return BigInt.zero()
  }
  return convertAssets(r.tokensToBurn, finalEvePrice)
}

/**
 * The first batch id `queueUpkeepStatus` cannot reach. The view walks its own
 * bounded cursor first, then scans at most MAX_BATCH_SCAN batches from wherever
 * that lands — so its reach is two windows deep from the stored cursor, not
 * one. Both decide and classify go through this helper so "did the view have a
 * chance to see this?" has exactly one answer.
 */
export function onChainScanWindowEnd(s: State): BigInt {
  return peekAdvancedCursor(s, MAX_BATCH_SCAN).plus(MAX_BATCH_SCAN)
}

/**
 * Mirrors `QueueKeeperExecutor._peekAdvancedCursor`. scanLimit bounds how far
 * the cursor may walk: pass MAX_BATCH_SCAN to reproduce what the executor will
 * do; pass zero for an unbounded off-chain walk.
 */
export function peekAdvancedCursor(s: State, scanLimit: BigInt): BigInt {
  let cursor = s.nextBatchIdToProcess
  let stop = s.currentBatchId
  if (scanLimit > BigInt.zero() && cursor.plus(scanLimit) < stop) {
    stop = cursor.plus(scanLimit)
  }
  while (cursor < stop && isBatchSkippable(s, cursor)) {
    cursor = cursor.plus(BigInt.fromI32(1))
  }
  return cursor
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
    return none('executor or a protocol contract is paused')
  }

  const fullCursor = peekAdvancedCursor(s, BigInt.zero())
  const windowEnd = onChainScanWindowEnd(s)

  // Process the oldest batch with an affordable prefix. Full scan.
  for (let id = fullCursor; id < s.currentBatchId; id = id.plus(BigInt.fromI32(1))) {
    if (isBatchSkippable(s, id)) continue
    const affordable = affordableRequests(s, id)
    if (affordable == BigInt.zero()) continue
    return new Decision(
      ActionProcessRequests,
      id,
      affordable,
      'batch ' +
        id.toString() +
        ' has ' +
        affordable.toString() +
        ' affordable requests within a controller balance of ' +
        s.controllerBalance.toString() +
        ' wei',
      id >= windowEnd
    )
  }

  // PriceBatch is accepted even when ProcessRequests was also due, and would
  // grow the live-priced set instead of settling. If the process walk did not
  // finish, we cannot know that nothing was affordable — skip pricing this
  // tick. AdvanceCursor is still safe: it only walks skippable batches we
  // already have headers for.
  if (!scanTruncated(s)) {
    const b = getBatch(s, s.currentBatchId)
    if (b != null && b.unprocessedCount > BigInt.zero() && s.now >= b.createdAt) {
      const age = s.now.minus(b.createdAt)
      if (age >= s.minBatchAge) {
        return new Decision(
          ActionPriceBatch,
          s.currentBatchId,
          BigInt.zero(),
          'current batch ' +
            s.currentBatchId.toString() +
            ' has ' +
            b.unprocessedCount.toString() +
            ' unprocessed requests and is ' +
            age.toString() +
            's old (minBatchAge ' +
            s.minBatchAge.toString() +
            's)',
          false
        )
      }
    }
  }

  // Advance the cursor past dead batches — capped at what the executor can
  // actually reach in one execution.
  const boundedCursor = peekAdvancedCursor(s, MAX_BATCH_SCAN)
  if (boundedCursor > s.nextBatchIdToProcess) {
    return new Decision(
      ActionAdvanceCursor,
      boundedCursor,
      BigInt.zero(),
      'cursor can advance from ' +
        s.nextBatchIdToProcess.toString() +
        ' to ' +
        boundedCursor.toString() +
        ' past fully-processed or expired batches',
      false
    )
  }

  return none('no batch needs pricing, processing, or a cursor advance')
}

function none(reason: string): Decision {
  return new Decision(ActionNone, BigInt.zero(), BigInt.zero(), reason, false)
}
