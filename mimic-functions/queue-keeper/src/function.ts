/**
 * W1 queue-keeper — Mimic function.
 *
 * Each tick: read queue state from the chain (through Mimic's oracle-backed
 * EvmCall queries), decide the next action with the ported decision engine,
 * and either emit a perform EvmCall intent executed from the operator's smart
 * account, or emit nothing.
 *
 * Ported from the CRE-era Go workflow and the Gelato W3F. What changed:
 * no DON report, no envelope, no 15-read budget (the scan is still bounded by
 * inputs.maxBatches so a long stall cannot produce an unbounded tick), and
 * reads now go through oracle-signed EvmCall queries. What did not change:
 * the decision engine, the "no amounts in payloads" rule, and the on-chain
 * cross-check with divergence classification.
 */

import { BigInt, Bytes, environment, log, TokenAmount } from '@mimicprotocol/lib-ts'
import { DenominationToken } from '@mimicprotocol/lib-ts'

import { IExitQueue } from './types/IExitQueue'
import { Pausable } from './types/Pausable'
import { QueueKeeperExecutor } from './types/QueueKeeperExecutor'
import { ActionNone, Batch, decide, Request, requestCost, State } from './decide'
import { classify, unexplained, UpkeepStatus } from './divergence'
import { encode, Params } from './params'
import { inputs } from './types'

export default function main(): void {
  const executor = new QueueKeeperExecutor(inputs.executor, inputs.chainId)
  const exitQueue = new IExitQueue(inputs.exitQueue, inputs.chainId)
  const controller = new Pausable(inputs.controller, inputs.chainId)

  const state = readState(executor, exitQueue, controller)
  const decision = decide(state)

  // Cross-check against the gas-bounded on-chain view. A failure here loses
  // the cross-check, not the decision — the keeper must keep working.
  let divergenceClass = 'unavailable'
  const status = executor.queueUpkeepStatus()
  if (!status.isError) {
    const oc = status.unwrap()
    const d = classify(decision, new UpkeepStatus(oc.action, oc.batchId, oc.count), state)
    divergenceClass = d.klass
    if (unexplained(d)) {
      log.error('W1 divergence from on-chain view is unexplained: ' + d.explanation)
    } else {
      log.info('W1 cross-check: ' + d.klass + ' — ' + d.explanation)
    }
  } else {
    log.warning('queueUpkeepStatus cross-check unavailable: ' + status.error)
  }

  log.info(
    'W1 queue-keeper: action=' +
      actionName(decision.action) +
      ' batch=' +
      decision.batchId.toString() +
      ' end=' +
      decision.endIndex.toString() +
      ' divergence=' +
      divergenceClass +
      ' — ' +
      decision.reason
  )

  if (decision.action == ActionNone) return

  // perform's params bytes come from params.encode, whose surface admits only
  // a batch id and an index range: no ETH amount can enter the payload. The
  // generated wrapper builds the exact calldata (selector + action + params),
  // byte-identical to what the on-chain checker() would emit.
  const paramsBytes = encode(
    decision.action,
    new Params(decision.action, decision.batchId, BigInt.zero(), decision.endIndex)
  )

  const fee = TokenAmount.fromStringDecimal(DenominationToken.USD(), inputs.maxFee)

  executor.perform(decision.action, Bytes.fromUint8Array(paramsBytes)).addUser(inputs.smartAccount).build().send(fee)
}

function actionName(a: u8): string {
  if (a == 0) return 'None'
  if (a == 1) return 'PriceBatch'
  if (a == 2) return 'ProcessRequests'
  if (a == 3) return 'AdvanceCursor'
  return 'Action(' + a.toString() + ')'
}

/**
 * Reads the full decision snapshot. Mirrors the Gelato-era readState, phase
 * for phase: config reads, then a batchInfo/unprocessedUsersCount walk, then
 * user lists and requestInfo for the first batch with an affordable prefix.
 */
function readState(executor: QueueKeeperExecutor, exitQueue: IExitQueue, controller: Pausable): State {
  const context = environment.getContext()
  // The runner hands us a wall-clock millisecond timestamp; the queue's batch
  // and pricing timestamps are block-time seconds. Every age and window
  // comparison in decide() works in seconds, so convert once, here.
  const now = BigInt.fromString(context.timestamp.toString()).div(BigInt.fromI32(1000))

  const maxBatches = inputs.maxBatches > 0 ? inputs.maxBatches : 250
  const maxRequests = inputs.maxRequestsPerBatch > 0 ? inputs.maxRequestsPerBatch : 50

  // Pause fan-out: the executor plus the contracts it drives. unwrapOr(true)
  // treats an unreadable pause flag as paused — fail closed.
  const executorPaused = executor.paused().unwrapOr(true)
  const controllerPaused = controller.paused().unwrapOr(true)
  const exitQueuePaused = new Pausable(inputs.exitQueue, inputs.chainId).paused().unwrapOr(true)
  const paused = executorPaused || controllerPaused || exitQueuePaused

  // A paused protocol means every action reverts — do not read anything else.
  if (paused) {
    return new State(
      now,
      true,
      BigInt.zero(),
      BigInt.zero(),
      BigInt.zero(),
      BigInt.zero(),
      BigInt.zero(),
      BigInt.zero(),
      new Map<string, Batch>(),
      BigInt.zero()
    )
  }

  const currentBatchId = exitQueue.currentBatchId().unwrap()
  const maxProcessing = exitQueue.MAX_BATCH_PROCESSING_TIME().unwrap()
  const cursor = executor.nextBatchIdToProcess().unwrap()
  const minBatchAge = executor.minBatchAge().unwrap()
  const maxUsersPerUpkeep = executor.maxUsersPerUpkeep().unwrap()

  const state = new State(
    now,
    paused,
    currentBatchId,
    cursor,
    BigInt.zero(), // controllerBalance, filled below
    maxProcessing,
    minBatchAge,
    maxUsersPerUpkeep,
    new Map<string, Batch>(),
    BigInt.zero()
  )

  // The Controller's own balance — the only budget affordability depends on.
  const balanceResult = environment.getNativeTokenBalance(inputs.chainId, inputs.controller)
  if (balanceResult.isError) {
    log.warning('controller balance unavailable: ' + balanceResult.error)
    return state
  }
  state.controllerBalance = balanceResult.unwrap()

  // Scan range: the executor's cursor through the current batch (inclusive of
  // the current one, because PriceBatch needs its age and unprocessed count).
  let first = cursor
  const last = currentBatchId
  if (first > last) first = last
  const scanEnd = first.plus(BigInt.fromI32(maxBatches))

  // Phase 1: batchInfo + unprocessedUsersCount for every batch in range.
  const ids: BigInt[] = []
  for (let id = first; id <= last && id < scanEnd; id = id.plus(BigInt.fromI32(1))) {
    ids.push(id)
    const info = exitQueue.batchInfo(id).unwrap()
    const unprocessed = exitQueue.unprocessedUsersCount(id).unwrap()
    state.batches.set(
      id.toString(),
      new Batch(
        id,
        info.canBeProcessed,
        info.finalEvePrice,
        info.totalTokensToBurn,
        info.createdAt,
        info.pricedAt,
        unprocessed,
        new Array<Request>(0)
      )
    )
  }

  // Record where the scan actually stopped, so "found nothing" is
  // distinguishable from "did not look". scanTruncatedAt is the last batch id
  // actually fetched; zero means the scan reached the current batch.
  if (ids.length > 0) {
    const reached = ids[ids.length - 1]
    if (reached < currentBatchId) state.scanTruncatedAt = reached
  }

  // Phase 2/3: user lists + requestInfo, in the same order decide walks.
  // Empty and expired batches are skippable (no user read). Unpriced batches
  // need PriceBatch, not users. A priced in-window head whose prefix is 0
  // (first request overruns the Controller) is not skippable — the view
  // continues — so we load the next candidate rather than stopping early.
  for (let i = 0; i < ids.length; i++) {
    const id = ids[i]
    const key = id.toString()
    if (!state.batches.has(key)) continue
    const b = state.batches.get(key)
    if (b == null) continue
    if (!b.canBeProcessed || b.unprocessedCount == BigInt.zero()) continue
    if (b.pricedAt > BigInt.zero() && state.now > b.pricedAt.plus(maxProcessing)) continue

    let limit = b.unprocessedCount
    if (limit > maxUsersPerUpkeep) limit = maxUsersPerUpkeep
    if (limit > BigInt.fromI32(maxRequests)) limit = BigInt.fromI32(maxRequests)

    const users = exitQueue.unprocessedUsers(id, BigInt.zero(), limit).unwrap()
    if (users.length == 0) continue

    const requests = new Array<Request>(users.length)
    for (let u = 0; u < users.length; u++) {
      const r = exitQueue.requestInfo(id, users[u]).unwrap()
      requests[u] = new Request(
        users[u].toString(),
        r.processed,
        r.closedDueToSlippage,
        r.evePriceAtRequestTime,
        r.tokensToBurn,
        r.priceTolerance
      )
    }
    b.requests = requests

    // decide will pick this batch if it has an affordable prefix; later ids
    // cannot be chosen this tick — stop scanning once one is found.
    let cumulative = BigInt.zero()
    let affordable = BigInt.zero()
    for (let q = 0; q < requests.length; q++) {
      const cost = requestCost(b.finalEvePrice, requests[q])
      if (cumulative.plus(cost) > state.controllerBalance) break
      cumulative = cumulative.plus(cost)
      affordable = affordable.plus(BigInt.fromI32(1))
    }
    if (affordable > BigInt.zero()) break
  }

  return state
}
