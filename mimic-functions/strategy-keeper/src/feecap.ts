/**
 * Per-action max fee.
 *
 * W2 never touches the payload; what it chooses is how much the intent may
 * pay for it. A solver quotes ≈ gasUsed × gas price × its markup, so a fee
 * cap is a gas-price ceiling: above it no solver fills, the intent lapses and
 * the next 5-minute tick retries at no cost beyond an idle tick. Waiting is
 * only rational where it is cheap, so each action is capped by what waiting
 * it out costs. The numbers and the data they come from are in
 * docs/MIMIC_CUTOVER.md ("Fee caps").
 *
 *   Rebalance, Sync        flat gwei ceiling
 *   WithdrawShortfall      gwei ceiling, ramping to an urgent ceiling as the
 *                          earliest in-window batch nears expiry — a priced
 *                          exit is committed and strands at pricedAt + 3 days
 *   DepositExcess,         share of the amount moved — the only cost of
 *   Harvest, ExitLiquidity waiting is yield on idle ETH
 *   anything else          flat maxFee (undecodable payload, unknown action)
 *
 * Every cap is converted to USD at the oracle's native-token price and
 * clamped to maxFee, which stays the absolute ceiling on any tick.
 *
 * A Rebalance also takes the withdrawal ramp: checker() returns only the
 * highest-priority action, so while a Rebalance waits for cheap gas the
 * WithdrawShortfall behind it is invisible. Pricing the Rebalance for the
 * withdrawal's deadline is what keeps a gas spike from stranding an exit.
 */

import { BigInt, ChainId, DenominationToken, ERC20Token, Result, TokenAmount } from '@mimicprotocol/lib-ts'

import { IExitQueue } from './types/IExitQueue'
import { IQueueKeeperExecutor } from './types/IQueueKeeperExecutor'
import { IRegistry } from './types/IRegistry'
import { StrategyKeeperExecutor } from './types/StrategyKeeperExecutor'
import { StrategyManager } from './types/StrategyManager'
import {
  ActionDepositExcess,
  ActionHarvestPerformanceFees,
  ActionProvideExitLiquidity,
  ActionRebalance,
  ActionSync,
  ActionWithdrawShortfall,
} from './actions'
import {
  executorRegistry,
  EXIT_QUEUE_KEY,
  QUEUE_KEEPER_EXECUTOR_KEY,
  resolveKey,
  STRATEGY_MANAGER_KEY,
} from './registry'
import { inputs } from './types'

// Worst gasUsed per strategy touched across W2's mainnet settlements
// 2026-09-20..23 (3 strategies), rounded up to 10k. A gwei ceiling only means
// something against the gas it multiplies: a short budget silently tightens
// the ceiling, so these scale with the strategies the action touches rather
// than baking in today's three.
const REBALANCE_GAS_PER_STRATEGY: i64 = 1690000 // one strategy per tx: 1,445,909..1,687,026
const SYNC_GAS_PER_STRATEGY: i64 = 210000 // three strategies: 519,276..600,467
const WITHDRAW_GAS_PER_STRATEGY: i64 = 980000 // three strategies: 2,935,628 (one sample)

// Solver fee over the gas it spent (gasUsed × effective gas price, at the
// block's ETH price), same settlements: 1.03..1.41 (plus one 0.74, where the
// base fee rose after the quote). Sized to the worst, so a fill that lands
// under the ceiling is never refused for markup.
const SOLVER_MARKUP_BPS: i64 = 14500
const BPS: i64 = 10000

const GWEI_DECIMALS: u8 = 9
const USD_DECIMALS: u8 = 18
const SECONDS_PER_HOUR: i64 = 3600

/** Trigger inputs, validated once per tick. */
export class FeePolicy {
  private constructor(
    readonly maxFeeUsd: BigInt,
    readonly rebalanceCeiling: BigInt,
    readonly syncCeiling: BigInt,
    readonly withdrawCeiling: BigInt,
    readonly withdrawUrgentCeiling: BigInt,
    readonly rampStart: i64,
    readonly rampEnd: i64,
    readonly amountFeeBps: i64
  ) {}

  /**
   * A malformed input would otherwise reach BigInt.fromStringDecimal, which
   * aborts the module — every tick dead with no log line naming the field.
   */
  static fromInputs(): Result<FeePolicy, string> {
    const maxFee = parseDecimal('maxFee', inputs.maxFee, USD_DECIMALS)
    if (maxFee.isError) return Result.err<FeePolicy, string>(maxFee.error)
    const rebalance = parseGwei('rebalanceMaxGwei', inputs.rebalanceMaxGwei)
    if (rebalance.isError) return Result.err<FeePolicy, string>(rebalance.error)
    const sync = parseGwei('syncMaxGwei', inputs.syncMaxGwei)
    if (sync.isError) return Result.err<FeePolicy, string>(sync.error)
    const withdraw = parseGwei('withdrawMaxGwei', inputs.withdrawMaxGwei)
    if (withdraw.isError) return Result.err<FeePolicy, string>(withdraw.error)
    const urgent = parseGwei('withdrawUrgentMaxGwei', inputs.withdrawUrgentMaxGwei)
    if (urgent.isError) return Result.err<FeePolicy, string>(urgent.error)

    if (urgent.unwrap().lt(withdraw.unwrap())) {
      return Result.err<FeePolicy, string>(
        'withdrawUrgentMaxGwei is below withdrawMaxGwei — the deadline ramp would lower the ceiling as expiry nears'
      )
    }
    if (inputs.withdrawRampStartHours <= inputs.withdrawRampEndHours) {
      return Result.err<FeePolicy, string>(
        'withdrawRampStartHours must exceed withdrawRampEndHours — the ramp runs from start down to end hours before expiry'
      )
    }
    if (inputs.amountFeeBps == 0 || inputs.amountFeeBps > <u32>BPS) {
      return Result.err<FeePolicy, string>(
        'amountFeeBps must be in 1..10000 — 0 never fills a deposit, above 10000 pays more than the amount moved'
      )
    }

    return Result.ok<FeePolicy, string>(
      new FeePolicy(
        maxFee.unwrap(),
        rebalance.unwrap(),
        sync.unwrap(),
        withdraw.unwrap(),
        urgent.unwrap(),
        (inputs.withdrawRampStartHours as i64) * SECONDS_PER_HOUR,
        (inputs.withdrawRampEndHours as i64) * SECONDS_PER_HOUR,
        inputs.amountFeeBps as i64
      )
    )
  }

  get maxFee(): TokenAmount {
    return TokenAmount.fromBigInt(DenominationToken.USD(), this.maxFeeUsd)
  }
}

export class FeeCap {
  constructor(
    readonly fee: TokenAmount,
    readonly basis: string
  ) {}
}

/**
 * The max fee for relaying `action`. `selectedStrategies` is the Rebalance
 * selection suppression already walked (`!paused ∧ ¬healthy`); other actions
 * ignore it. `now` is block-comparable seconds.
 */
export function feeCap(
  executor: StrategyKeeperExecutor,
  chainId: ChainId,
  action: i32,
  selectedStrategies: i32,
  policy: FeePolicy,
  now: i64
): Result<FeeCap, string> {
  if (action == ActionRebalance || action == ActionWithdrawShortfall || action == ActionSync) {
    return gweiCap(executor, chainId, action, selectedStrategies, policy, now)
  }
  if (action == ActionDepositExcess || action == ActionHarvestPerformanceFees || action == ActionProvideExitLiquidity) {
    return amountCap(executor, chainId, action, policy)
  }
  return Result.ok<FeeCap, string>(
    new FeeCap(
      policy.maxFee,
      'flat maxFee $' + policy.maxFee.amount.toStringDecimal(USD_DECIMALS) + ' (no action to price)'
    )
  )
}

function gweiCap(
  executor: StrategyKeeperExecutor,
  chainId: ChainId,
  action: i32,
  selectedStrategies: i32,
  policy: FeePolicy,
  now: i64
): Result<FeeCap, string> {
  const registryResult = executorRegistry(executor, chainId)
  if (registryResult.isError) return Result.err<FeeCap, string>(registryResult.error)
  const registry = registryResult.unwrap()

  let gas: i64 = 0
  let ceiling = BigInt.zero()
  let basis = ''

  if (action == ActionRebalance) {
    // An empty selection is oracle skew against the checker (suppression
    // relays it); price the one strategy the contract must have seen.
    const strategies = selectedStrategies > 0 ? selectedStrategies : 1
    gas = REBALANCE_GAS_PER_STRATEGY * (strategies as i64)
    ceiling = policy.rebalanceCeiling
    basis = strategies.toString() + ' selected'

    const deadline = earliestDeadline(executor, registry, chainId, now)
    if (deadline.isError) return Result.err<FeeCap, string>(deadline.error)
    if (deadline.unwrap() >= 0) {
      const ramp = withdrawCeiling(policy, deadline.unwrap() - now)
      if (ramp.gt(ceiling)) {
        ceiling = ramp
        basis += ', raised for the withdrawal it masks: batch expires in ' + formatHours(deadline.unwrap() - now)
      }
    }
  } else {
    const count = strategyCount(registry, chainId)
    if (count.isError) return Result.err<FeeCap, string>(count.error)
    const strategies = count.unwrap() > 0 ? count.unwrap() : 1
    basis = strategies.toString() + ' strategies'

    if (action == ActionSync) {
      gas = SYNC_GAS_PER_STRATEGY * (strategies as i64)
      ceiling = policy.syncCeiling
    } else {
      gas = WITHDRAW_GAS_PER_STRATEGY * (strategies as i64)
      const deadline = earliestDeadline(executor, registry, chainId, now)
      if (deadline.isError) return Result.err<FeeCap, string>(deadline.error)
      if (deadline.unwrap() >= 0) {
        ceiling = withdrawCeiling(policy, deadline.unwrap() - now)
        basis += ', batch expires in ' + formatHours(deadline.unwrap() - now)
      } else {
        // No in-window batch with unprocessed users: oracle skew against the
        // checker. No deadline is known, so no urgency is claimed.
        ceiling = policy.withdrawCeiling
        basis += ', no expiring batch seen'
      }
    }
  }

  const capWei = BigInt.fromI64(gas).times(ceiling).times(BigInt.fromI64(SOLVER_MARKUP_BPS)).div(BigInt.fromI64(BPS))
  return toUsdCap(
    chainId,
    capWei,
    ceiling.toStringDecimal(GWEI_DECIMALS) + ' gwei × ' + gas.toString() + ' gas (' + basis + ') × 1.45 markup',
    policy
  )
}

/**
 * Deposit, harvest and exit-liquidity cost nothing to defer but yield on
 * idle ETH, so the cap is a share of the amount moved. The amount is the
 * contract's own estimate from the same helpers checker() uses; it prices
 * the fee and nothing else — the payload is still checker()'s bytes, and
 * perform re-derives every amount on-chain (CLAUDE.md §1).
 */
function amountCap(
  executor: StrategyKeeperExecutor,
  chainId: ChainId,
  action: i32,
  policy: FeePolicy
): Result<FeeCap, string> {
  const statusResult = executor.strategyUpkeepStatus()
  if (statusResult.isError) return Result.err<FeeCap, string>('strategyUpkeepStatus(): ' + statusResult.error)
  const status = statusResult.unwrap()
  if ((status.action as i32) != action) {
    // checker() and strategyUpkeepStatus() share one helper; disagreement is
    // state moving between the two reads, and the amount belongs to a
    // different action than the payload.
    return Result.err<FeeCap, string>(
      'strategyUpkeepStatus() reports action ' +
        status.action.toString() +
        ' but checker() built ' +
        action.toString() +
        ' — the amount would price the wrong work'
    )
  }

  const capWei = status.amount.times(BigInt.fromI64(policy.amountFeeBps)).div(BigInt.fromI64(BPS))
  return toUsdCap(
    chainId,
    capWei,
    policy.amountFeeBps.toString() + ' bps of ' + status.amount.toStringDecimal(18) + ' ETH',
    policy
  )
}

function toUsdCap(chainId: ChainId, capWei: BigInt, basis: string, policy: FeePolicy): Result<FeeCap, string> {
  const usdResult = TokenAmount.fromBigInt(ERC20Token.native(chainId), capWei).toUsd()
  if (usdResult.isError) return Result.err<FeeCap, string>('native token price: ' + usdResult.error)
  const usd = usdResult.unwrap().value

  const described = basis + ' = ' + capWei.toStringDecimal(18) + ' ETH = $' + usd.toStringDecimal(USD_DECIMALS)
  if (usd.gt(policy.maxFeeUsd)) {
    return Result.ok<FeeCap, string>(
      new FeeCap(policy.maxFee, described + ', clamped to maxFee $' + policy.maxFeeUsd.toStringDecimal(USD_DECIMALS))
    )
  }
  return Result.ok<FeeCap, string>(new FeeCap(TokenAmount.fromBigInt(DenominationToken.USD(), usd), described))
}

/**
 * Floor while expiry is far, the urgent ceiling from `rampEnd` hours out,
 * linear in between. A priced exit that is not funded before expiry cannot
 * be recovered: pullRequest reverts ExitQueueBatchExpired and the user closes
 * for their EVE with no ETH.
 */
function withdrawCeiling(policy: FeePolicy, secondsLeft: i64): BigInt {
  if (secondsLeft <= policy.rampEnd) return policy.withdrawUrgentCeiling
  if (secondsLeft >= policy.rampStart) return policy.withdrawCeiling
  const span = policy.withdrawUrgentCeiling.minus(policy.withdrawCeiling)
  return policy.withdrawCeiling.plus(
    span.times(BigInt.fromI64(policy.rampStart - secondsLeft)).div(BigInt.fromI64(policy.rampStart - policy.rampEnd))
  )
}

/**
 * Earliest `pricedAt + MAX_BATCH_PROCESSING_TIME` among the batches the
 * executor's `_pendingRedemptionNeedsETH` still counts, or -1 when none.
 * Same window as the contract — [nextLiveBatchIdToProcess, currentBatchId)
 * capped at the executor's MAX_BATCH_SCAN (CLAUDE.md §3: W2 may not scan
 * deeper). A batch whose remaining requests are all out of tolerance costs 0
 * on-chain but still counts here: telling them apart takes a requestInfo per
 * user, and the error only ever makes the keeper pay sooner, never strand.
 */
function earliestDeadline(
  executor: StrategyKeeperExecutor,
  registry: IRegistry,
  chainId: ChainId,
  now: i64
): Result<i64, string> {
  const queueAddress = resolveKey(registry, EXIT_QUEUE_KEY, 'EXIT_QUEUE')
  if (queueAddress.isError) return Result.err<i64, string>(queueAddress.error)
  const keeperAddress = resolveKey(registry, QUEUE_KEEPER_EXECUTOR_KEY, 'QUEUE_KEEPER_EXECUTOR')
  if (keeperAddress.isError) return Result.err<i64, string>(keeperAddress.error)
  const queue = new IExitQueue(queueAddress.unwrap(), chainId)
  const queueKeeper = new IQueueKeeperExecutor(keeperAddress.unwrap(), chainId)

  const scanResult = executor.MAX_BATCH_SCAN()
  if (scanResult.isError) return Result.err<i64, string>('MAX_BATCH_SCAN(): ' + scanResult.error)
  const cursorResult = queueKeeper.nextLiveBatchIdToProcess()
  if (cursorResult.isError) return Result.err<i64, string>('nextLiveBatchIdToProcess(): ' + cursorResult.error)
  const currentResult = queue.currentBatchId()
  if (currentResult.isError) return Result.err<i64, string>('currentBatchId(): ' + currentResult.error)
  const windowResult = queue.MAX_BATCH_PROCESSING_TIME()
  if (windowResult.isError) return Result.err<i64, string>('MAX_BATCH_PROCESSING_TIME(): ' + windowResult.error)

  const cursor = cursorResult.unwrap().toI64()
  const scanEnd = cursor + scanResult.unwrap().toI64()
  const current = currentResult.unwrap().toI64()
  const end = current < scanEnd ? current : scanEnd
  const window = windowResult.unwrap().toI64()

  let earliest: i64 = -1
  for (let id = cursor; id < end; id++) {
    const batchId = BigInt.fromI64(id)
    const infoResult = queue.batchInfo(batchId)
    if (infoResult.isError) return Result.err<i64, string>('batchInfo(' + id.toString() + '): ' + infoResult.error)
    const info = infoResult.unwrap()
    if (!info.canBeProcessed) continue
    const pricedAt = info.pricedAt.toI64()
    // canBeProcessed with pricedAt 0 has no expiry on-chain either.
    if (pricedAt == 0) continue
    const deadline = pricedAt + window
    // `_batchSettlementCost`: block.timestamp > pricedAt + window costs 0.
    if (now > deadline) continue

    const countResult = queue.unprocessedUsersCount(batchId)
    if (countResult.isError) {
      return Result.err<i64, string>('unprocessedUsersCount(' + id.toString() + '): ' + countResult.error)
    }
    if (countResult.unwrap().isZero()) continue
    if (earliest < 0 || deadline < earliest) earliest = deadline
  }
  return Result.ok<i64, string>(earliest)
}

function strategyCount(registry: IRegistry, chainId: ChainId): Result<i32, string> {
  const managerResult = resolveKey(registry, STRATEGY_MANAGER_KEY, 'STRATEGY_MANAGER')
  if (managerResult.isError) return Result.err<i32, string>(managerResult.error)
  const strategiesResult = new StrategyManager(managerResult.unwrap(), chainId).strategies()
  if (strategiesResult.isError) return Result.err<i32, string>('strategies(): ' + strategiesResult.error)
  return Result.ok<i32, string>(strategiesResult.unwrap().length)
}

function parseGwei(field: string, value: string): Result<BigInt, string> {
  const result = parseDecimal(field, value, GWEI_DECIMALS)
  if (result.isError) return result
  if (result.unwrap().isZero()) {
    return Result.err<BigInt, string>(
      field + ' is 0 — a zero gas-price ceiling never fills, the action would never run'
    )
  }
  return result
}

function parseDecimal(field: string, value: string, decimals: u8): Result<BigInt, string> {
  const dot = value.indexOf('.')
  let valid = value.length > 0 && dot != 0 && dot != value.length - 1 && value.lastIndexOf('.') == dot
  if (valid && dot > 0) valid = value.length - dot - 1 <= (decimals as i32)
  for (let i = 0; valid && i < value.length; i++) {
    const c = value.charCodeAt(i)
    if (i != dot && (c < 48 || c > 57)) valid = false
  }
  if (!valid) {
    return Result.err<BigInt, string>(
      field + ' "' + value + '" is not a plain decimal with at most ' + decimals.toString() + ' fractional digits'
    )
  }
  return Result.ok<BigInt, string>(BigInt.fromStringDecimal(value, decimals))
}

function formatHours(seconds: i64): string {
  const tenths = (seconds * 10) / SECONDS_PER_HOUR
  return (tenths / 10).toString() + '.' + (tenths % 10).toString() + 'h'
}
