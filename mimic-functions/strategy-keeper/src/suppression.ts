/**
 * No-op Rebalance suppression (keepers#27).
 *
 * The executor's Rebalance trigger is `!paused && !isHealthy()`, but
 * `UniCLStrat.rebalance()` additionally requires `_isCalm()`. A strategy
 * that is unhealthy *only* because the market is not calm makes the whole
 * batch revert on-chain (`UniCLStratNotCalm`, swallowed by StrategyManager's
 * try/catch) while the tick reports success having moved nothing — burnt gas
 * every 5 minutes for as long as the turbulence lasts.
 *
 * The suppression predicate:
 *
 *   suppress ⟺ action == Rebalance
 *            ∧ ∀ s ∈ strategyManager.strategies():
 *                (!s.paused() ∧ ¬s.isHealthy())  ⇒  ¬calm(s)
 *
 * `rebalance()` reverts on `!_isCalm()` regardless of the range/threshold
 * reason for `!isHealthy()`, so a not-calm selected strategy cannot move a
 * wei; any selected strategy with `calm ∧ ¬healthy` is doing real work and
 * the batch is relayed untouched. The payload is never rebuilt or
 * re-encoded — suppression only ever chooses between relaying the
 * contract's own bytes and emitting nothing.
 */

import { Address, ChainId, Result } from '@mimicprotocol/lib-ts'

import { IUniswapV3Pool } from './types/IUniswapV3Pool'
import { StrategyKeeperExecutor } from './types/StrategyKeeperExecutor'
import { StrategyManager } from './types/StrategyManager'
import { UniCLStrat } from './types/UniCLStrat'
import { executorRegistry, resolveKey, STRATEGY_MANAGER_KEY } from './registry'

/** The observed calm inputs for one `!paused ∧ ¬healthy` strategy. */
export class CalmCheck {
  constructor(
    readonly strategy: Address,
    readonly spotTick: i64,
    readonly twapTick: i64,
    readonly shortTwapTick: i64,
    readonly maxDeviation: i64,
    readonly calm: bool
  ) {}

  describe(): string {
    return (
      this.strategy.toHexString() +
      ' spot=' +
      this.spotTick.toString() +
      ' twap=' +
      this.twapTick.toString() +
      ' shortTwap=' +
      this.shortTwapTick.toString() +
      ' maxDeviation=' +
      this.maxDeviation.toString() +
      ' spotDeviation=' +
      (this.spotTick - this.twapTick).toString() +
      ' shortTwapDeviation=' +
      (this.shortTwapTick - this.twapTick).toString() +
      ' calm=' +
      this.calm.toString()
    )
  }
}

export class SuppressionVerdict {
  private constructor(
    readonly suppress: bool,
    readonly checks: CalmCheck[],
    readonly error: string
  ) {}

  static readError(error: string): SuppressionVerdict {
    return new SuppressionVerdict(false, [], error)
  }

  static evaluated(suppress: bool, checks: CalmCheck[]): SuppressionVerdict {
    return new SuppressionVerdict(suppress, checks, '')
  }
}

/**
 * Evaluates the suppression predicate for a Rebalance tick. Any oracle read
 * failure — including a pool `observe` that reverts because it cannot serve
 * the window (on-chain that means `calm == false`, but off-chain the revert
 * is indistinguishable from an RPC failure) — yields a readError verdict and
 * the tick emits nothing. Skew between the checker read and these reads is
 * inherent to the read-now/write-later split; a calm flip in between either
 * retries a suppressed-but-valid rebalance next tick or relays one that
 * reverts, exactly as today.
 */
export function evaluateSuppression(executor: StrategyKeeperExecutor, chainId: ChainId): SuppressionVerdict {
  const registryResult = executorRegistry(executor, chainId)
  if (registryResult.isError) return SuppressionVerdict.readError(registryResult.error)

  const managerResult = resolveKey(registryResult.unwrap(), STRATEGY_MANAGER_KEY, 'STRATEGY_MANAGER')
  if (managerResult.isError) return SuppressionVerdict.readError(managerResult.error)
  const strategyManager = new StrategyManager(managerResult.unwrap(), chainId)

  const strategiesResult = strategyManager.strategies()
  if (strategiesResult.isError) return SuppressionVerdict.readError('strategies(): ' + strategiesResult.error)
  const strategies = strategiesResult.unwrap()

  const checks: CalmCheck[] = []
  for (let i = 0; i < strategies.length; i++) {
    const strategy = new UniCLStrat(strategies[i], chainId)

    // Same selection as the executor's `_rebalanceNeeded`.
    const pausedResult = strategy.paused()
    if (pausedResult.isError) return SuppressionVerdict.readError('paused(): ' + pausedResult.error)
    if (pausedResult.unwrap()) continue
    const healthyResult = strategy.isHealthy()
    if (healthyResult.isError) return SuppressionVerdict.readError('isHealthy(): ' + healthyResult.error)
    if (healthyResult.unwrap()) continue

    const check = evaluateCalm(strategy, chainId)
    if (check.isError) return SuppressionVerdict.readError(check.error)
    checks.push(check.unwrap())
  }

  // An empty selection means the contract's scan and ours disagree (oracle
  // skew); relay and let perform's own `_rebalanceNeeded` guard decide.
  if (checks.length == 0) return SuppressionVerdict.evaluated(false, checks)

  for (let i = 0; i < checks.length; i++) {
    if (checks[i].calm) return SuppressionVerdict.evaluated(false, checks)
  }
  return SuppressionVerdict.evaluated(true, checks)
}

/** `_isCalm()` off-chain: spot tick and short TWAP both within ±maxTickDeviation of the long TWAP. */
function evaluateCalm(strategy: UniCLStrat, chainId: ChainId): Result<CalmCheck, string> {
  const poolResult = strategy.pool()
  if (poolResult.isError) return Result.err<CalmCheck, string>('pool(): ' + poolResult.error)
  const deviationResult = strategy.maxTickDeviation()
  if (deviationResult.isError) return Result.err<CalmCheck, string>('maxTickDeviation(): ' + deviationResult.error)
  const twapIntervalResult = strategy.twapInterval()
  if (twapIntervalResult.isError) return Result.err<CalmCheck, string>('twapInterval(): ' + twapIntervalResult.error)
  const shortIntervalResult = strategy.shortTwapInterval()
  if (shortIntervalResult.isError) {
    return Result.err<CalmCheck, string>('shortTwapInterval(): ' + shortIntervalResult.error)
  }

  const pool = new IUniswapV3Pool(poolResult.unwrap(), chainId)

  const slot0Result = pool.slot0()
  if (slot0Result.isError) return Result.err<CalmCheck, string>('slot0(): ' + slot0Result.error)
  const spotTick = slot0Result.unwrap().tick.toI64()

  const twapTickResult = observeMeanTick(pool, twapIntervalResult.unwrap())
  if (twapTickResult.isError) return Result.err<CalmCheck, string>('observe(twap): ' + twapTickResult.error)
  const shortTwapTickResult = observeMeanTick(pool, shortIntervalResult.unwrap())
  if (shortTwapTickResult.isError) {
    return Result.err<CalmCheck, string>('observe(shortTwap): ' + shortTwapTickResult.error)
  }

  const twapTick = twapTickResult.unwrap()
  const shortTwapTick = shortTwapTickResult.unwrap()
  const maxDeviation = deviationResult.unwrap().toI64()

  const minCalmTick = twapTick - maxDeviation
  const maxCalmTick = twapTick + maxDeviation
  const calm =
    spotTick >= minCalmTick && spotTick <= maxCalmTick && shortTwapTick >= minCalmTick && shortTwapTick <= maxCalmTick

  return Result.ok<CalmCheck, string>(
    new CalmCheck(strategy.address, spotTick, twapTick, shortTwapTick, maxDeviation, calm)
  )
}

/**
 * `TickUtils.tryMeanTick` / `meanTickFromCumulatives`: mean of the trailing
 * `interval` seconds, rounding toward negative infinity like Uniswap's
 * OracleLibrary. int56 cumulatives and their delta fit in i64. An observe
 * error is an unavailable TWAP — on-chain that forces `calm == false`.
 */
function observeMeanTick(pool: IUniswapV3Pool, interval: u32): Result<i64, string> {
  const result = pool.observe([interval, 0])
  if (result.isError) return Result.err<i64, string>(result.error)
  const cumulatives = result.unwrap().tickCumulatives
  if (cumulatives.length != 2) {
    return Result.err<i64, string>('expected 2 tick cumulatives, got ' + cumulatives.length.toString())
  }
  const delta = cumulatives[1].minus(cumulatives[0]).toI64()
  const span = interval as i64
  let tick = delta / span
  if (delta < 0 && delta % span != 0) tick--
  return Result.ok<i64, string>(tick)
}
