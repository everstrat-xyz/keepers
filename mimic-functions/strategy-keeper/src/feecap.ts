/**
 * Per-action gas gate and max fee.
 *
 * W2 never touches the payload; what it chooses is when to relay it and how
 * much the intent may pay. Waiting is only rational where it is cheap, so
 * each action is handled by what waiting it out costs. The numbers and the
 * data they come from are in docs/MIMIC_CUTOVER.md ("Gas gate and fee caps").
 *
 *   Rebalance, Sync        gwei ceiling
 *   WithdrawShortfall      gwei ceiling, ramping to an urgent ceiling as the
 *                          earliest in-window batch nears expiry — a priced
 *                          exit is committed and strands at pricedAt + 3 days
 *   DepositExcess,         share of the amount moved — the only cost of
 *   Harvest, ExitLiquidity waiting is yield on idle ETH
 *   anything else          flat maxFee (undecodable payload, unknown action)
 *
 * A gwei ceiling is enforced twice:
 *
 *   - gate: while the gas feed reads above the ceiling, nothing is emitted and
 *     the next 5-minute tick asks again. This is the precise check, and it
 *     needs no knowledge of how much gas the action burns.
 *   - cap: the intent's max fee is ceiling × feeCapGas. feeCapGas is a
 *     deliberately loose gas allowance, sized above the largest bill any W2
 *     action can run up, so it never has to track strategy changes. It bounds
 *     the solver at fill time — a spike between this read and settlement, or
 *     a solver pricing far above its gas — and is the only check while the
 *     feed is unavailable.
 *
 * A Rebalance also takes the withdrawal ramp: checker() returns only the
 * highest-priority action, so while a Rebalance waits for cheap gas the
 * WithdrawShortfall behind it is invisible. Gating the Rebalance by the
 * withdrawal's deadline is what keeps a gas spike from stranding an exit.
 */

import { Address, BigInt, ChainId, DenominationToken, ERC20Token, Result, TokenAmount } from '@mimicprotocol/lib-ts'

import { AggregatorV3Interface } from './types/AggregatorV3Interface'
import { IExitQueue } from './types/IExitQueue'
import { IQueueKeeperExecutor } from './types/IQueueKeeperExecutor'
import { IRegistry } from './types/IRegistry'
import { StrategyKeeperExecutor } from './types/StrategyKeeperExecutor'
import {
  ActionDepositExcess,
  ActionHarvestPerformanceFees,
  ActionProvideExitLiquidity,
  ActionRebalance,
  ActionSync,
  ActionWithdrawShortfall,
} from './actions'
import { executorRegistry, EXIT_QUEUE_KEY, QUEUE_KEEPER_EXECUTOR_KEY, resolveKey } from './registry'
import { inputs } from './types'

// Chainlink's mainnet Fast Gas / Gwei feed updated at most 2.01h apart over
// the 14 days to 2026-10-09 (637 updates, median gap 15 min). Older than this
// is a feed that has stopped, not one that is quiet.
const MAX_FEED_AGE_SECONDS: i64 = 3 * 3600

const BPS: i64 = 10000
const GWEI_DECIMALS: u8 = 9
const USD_DECIMALS: u8 = 18
const SECONDS_PER_HOUR: i64 = 3600

/** Trigger inputs, validated once per tick. */
export class FeePolicy {
  private constructor(
    readonly gasPriceFeed: Address,
    readonly feeCapGas: i64,
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

    if (inputs.feeCapGas == 0) {
      return Result.err<FeePolicy, string>('feeCapGas is 0 — every gas-priced intent would carry a zero max fee')
    }
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
        inputs.gasPriceFeed,
        inputs.feeCapGas as i64,
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

export enum Outcome {
  Relay,
  DeferGas,
  ReadError,
  ConfigError,
}

export class FeeDecision {
  private constructor(
    readonly outcome: Outcome,
    readonly fee: TokenAmount,
    readonly note: string
  ) {}

  static relay(fee: TokenAmount, note: string): FeeDecision {
    return new FeeDecision(Outcome.Relay, fee, note)
  }

  static defer(note: string): FeeDecision {
    return new FeeDecision(Outcome.DeferGas, TokenAmount.fromI32(DenominationToken.USD(), 0), note)
  }

  static readError(note: string): FeeDecision {
    return new FeeDecision(Outcome.ReadError, TokenAmount.fromI32(DenominationToken.USD(), 0), note)
  }

  static configError(note: string): FeeDecision {
    return new FeeDecision(Outcome.ConfigError, TokenAmount.fromI32(DenominationToken.USD(), 0), note)
  }
}

/** Whether, and under what max fee, to relay `action`. `now` is block-comparable seconds. */
export function decideFee(
  executor: StrategyKeeperExecutor,
  chainId: ChainId,
  action: i32,
  policy: FeePolicy,
  now: i64
): FeeDecision {
  if (action == ActionRebalance || action == ActionWithdrawShortfall || action == ActionSync) {
    return gasGated(executor, chainId, action, policy, now)
  }
  if (action == ActionDepositExcess || action == ActionHarvestPerformanceFees || action == ActionProvideExitLiquidity) {
    return amountCapped(executor, chainId, action, policy)
  }
  return FeeDecision.relay(
    policy.maxFee,
    'flat maxFee $' + policy.maxFeeUsd.toStringDecimal(USD_DECIMALS) + ' (no action to price)'
  )
}

function gasGated(
  executor: StrategyKeeperExecutor,
  chainId: ChainId,
  action: i32,
  policy: FeePolicy,
  now: i64
): FeeDecision {
  let ceiling = BigInt.zero()
  let basis = ''

  if (action == ActionSync) {
    ceiling = policy.syncCeiling
  } else {
    const deadline = earliestDeadline(executor, chainId, now)
    if (deadline.isError) return FeeDecision.readError(deadline.error)
    const secondsLeft = deadline.unwrap() >= 0 ? deadline.unwrap() - now : -1

    if (action == ActionRebalance) {
      ceiling = policy.rebalanceCeiling
      if (secondsLeft >= 0) {
        const ramp = withdrawCeiling(policy, secondsLeft)
        if (ramp.gt(ceiling)) {
          ceiling = ramp
          basis = ', raised for the withdrawal it masks: batch expires in ' + formatHours(secondsLeft)
        }
      }
    } else if (secondsLeft >= 0) {
      ceiling = withdrawCeiling(policy, secondsLeft)
      basis = ', batch expires in ' + formatHours(secondsLeft)
    } else {
      // No in-window batch with unprocessed users: oracle skew against the
      // checker. No deadline is known, so no urgency is claimed.
      ceiling = policy.withdrawCeiling
      basis = ', no expiring batch seen'
    }
  }
  const ceilingText = ceiling.toStringDecimal(GWEI_DECIMALS) + ' gwei'

  const reading = readGasPrice(policy, chainId, now)
  if (reading.isError) return FeeDecision.configError(reading.error)
  const gas = reading.unwrap()
  if (gas.available && gas.wei.gt(ceiling)) {
    return FeeDecision.defer(
      'gas feed ' + gas.wei.toStringDecimal(GWEI_DECIMALS) + ' gwei > ceiling ' + ceilingText + basis
    )
  }

  const capWei = BigInt.fromI64(policy.feeCapGas).times(ceiling)
  const usd = toUsd(chainId, capWei)
  if (usd.isError) return FeeDecision.readError(usd.error)

  const gate = gas.available
    ? 'gas feed ' + gas.wei.toStringDecimal(GWEI_DECIMALS) + ' gwei ≤ ceiling ' + ceilingText
    : 'gas feed unavailable (' + gas.reason + '), cap only at ceiling ' + ceilingText
  return FeeDecision.relay(
    TokenAmount.fromBigInt(DenominationToken.USD(), usd.unwrap()),
    gate +
      basis +
      '; max fee ' +
      ceilingText +
      ' × ' +
      policy.feeCapGas.toString() +
      ' gas = ' +
      capWei.toStringDecimal(18) +
      ' ETH = $' +
      usd.unwrap().toStringDecimal(USD_DECIMALS)
  )
}

/**
 * Deposit, harvest and exit-liquidity cost nothing to defer but yield on
 * idle ETH, so the cap is a share of the amount moved and the solver alone
 * decides whether gas is cheap enough. The amount is the contract's own
 * estimate from the same helpers checker() uses; it prices the fee and
 * nothing else — the payload is still checker()'s bytes, and perform
 * re-derives every amount on-chain (CLAUDE.md §1).
 */
function amountCapped(executor: StrategyKeeperExecutor, chainId: ChainId, action: i32, policy: FeePolicy): FeeDecision {
  const statusResult = executor.strategyUpkeepStatus()
  if (statusResult.isError) return FeeDecision.readError('strategyUpkeepStatus(): ' + statusResult.error)
  const status = statusResult.unwrap()
  if ((status.action as i32) != action) {
    // checker() and strategyUpkeepStatus() share one helper; disagreement is
    // state moving between the two reads, and the amount belongs to a
    // different action than the payload.
    return FeeDecision.readError(
      'strategyUpkeepStatus() reports action ' +
        status.action.toString() +
        ' but checker() built ' +
        action.toString() +
        ' — the amount would price the wrong work'
    )
  }

  const capWei = status.amount.times(BigInt.fromI64(policy.amountFeeBps)).div(BigInt.fromI64(BPS))
  const usd = toUsd(chainId, capWei)
  if (usd.isError) return FeeDecision.readError(usd.error)

  const described =
    'max fee ' +
    policy.amountFeeBps.toString() +
    ' bps of ' +
    status.amount.toStringDecimal(18) +
    ' ETH = ' +
    capWei.toStringDecimal(18) +
    ' ETH = $' +
    usd.unwrap().toStringDecimal(USD_DECIMALS)
  if (usd.unwrap().gt(policy.maxFeeUsd)) {
    return FeeDecision.relay(
      policy.maxFee,
      described + ', clamped to maxFee $' + policy.maxFeeUsd.toStringDecimal(USD_DECIMALS)
    )
  }
  return FeeDecision.relay(TokenAmount.fromBigInt(DenominationToken.USD(), usd.unwrap()), described)
}

class GasReading {
  constructor(
    readonly available: bool,
    readonly wei: BigInt,
    readonly reason: string
  ) {}
}

/**
 * The gas feed's current wei-per-gas, or an unavailable reading with the
 * reason. `block.basefee` is not an option: through eth_call it reads 0 on
 * Geth, reth and Tenderly alike, and an oracle read is an eth_call.
 *
 * An unavailable feed degrades to the cap alone rather than stopping the
 * keeper — a withdrawal deadline does not wait for Chainlink. The only error
 * is a feed that answers in the wrong unit: that is a misconfigured address,
 * and gating by it would silently defer forever or never.
 */
function readGasPrice(policy: FeePolicy, chainId: ChainId, now: i64): Result<GasReading, string> {
  if (policy.gasPriceFeed.equals(Address.zero())) {
    return Result.ok<GasReading, string>(new GasReading(false, BigInt.zero(), 'no gasPriceFeed configured'))
  }
  const feed = new AggregatorV3Interface(policy.gasPriceFeed, chainId)

  const decimals = feed.decimals()
  if (decimals.isError) {
    return Result.ok<GasReading, string>(new GasReading(false, BigInt.zero(), 'decimals(): ' + decimals.error))
  }
  if (decimals.unwrap() != 0) {
    return Result.err<GasReading, string>(
      'gasPriceFeed reports ' +
        decimals.unwrap().toString() +
        ' decimals — a gas feed answers wei per gas with 0; this looks like a price feed, and gating by it would misread every tick'
    )
  }

  const round = feed.latestRoundData()
  if (round.isError) {
    return Result.ok<GasReading, string>(new GasReading(false, BigInt.zero(), 'latestRoundData(): ' + round.error))
  }
  const answer = round.unwrap().answer
  if (answer.le(BigInt.zero())) {
    return Result.ok<GasReading, string>(new GasReading(false, BigInt.zero(), 'answer ' + answer.toString()))
  }
  const age = now - round.unwrap().updatedAt.toI64()
  if (age > MAX_FEED_AGE_SECONDS) {
    return Result.ok<GasReading, string>(
      new GasReading(false, BigInt.zero(), 'stale, updated ' + formatHours(age) + ' ago')
    )
  }
  return Result.ok<GasReading, string>(new GasReading(true, answer, ''))
}

function toUsd(chainId: ChainId, wei: BigInt): Result<BigInt, string> {
  const usdResult = TokenAmount.fromBigInt(ERC20Token.native(chainId), wei).toUsd()
  if (usdResult.isError) return Result.err<BigInt, string>('native token price: ' + usdResult.error)
  return Result.ok<BigInt, string>(usdResult.unwrap().value)
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
function earliestDeadline(executor: StrategyKeeperExecutor, chainId: ChainId, now: i64): Result<i64, string> {
  const registryResult = executorRegistry(executor, chainId)
  if (registryResult.isError) return Result.err<i64, string>(registryResult.error)
  const registry: IRegistry = registryResult.unwrap()

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
