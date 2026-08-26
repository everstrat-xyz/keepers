/**
 * Solmath — literal transcriptions of the contracts' `Math` library.
 *
 * The keeper has to reproduce on-chain affordability arithmetic off-chain, and
 * "close enough" is not good enough: a function that rounds differently from
 * the contract proposes work the contract then refuses (`KeeperExecutorNoUpkeepNeeded`
 * on every tick), which looks exactly like a broken keeper.
 *
 * BigInt division truncates toward zero and all inputs are non-negative, so it
 * matches EVM `/` exactly.
 */

import { BigInt } from '@mimicprotocol/lib-ts'

/** Math.NORMALIZATION_FACTOR (1e18). */
export const NORMALIZATION_FACTOR: BigInt = BigInt.fromI32(10).pow(18 as u8)

/** Math.SCALE_FACTOR (1e18) — the "100%" denominator for relative comparisons. */
export const SCALE_FACTOR: BigInt = BigInt.fromI32(10).pow(18 as u8)

/** Math.convertAssets: `(amount * price) / 1e18`. */
export function convertAssets(normalizedAmount: BigInt, normalizedPrice: BigInt): BigInt {
  return normalizedAmount.times(normalizedPrice).div(NORMALIZATION_FACTOR)
}

/**
 * Whether `difference` is in the range Math.isRelativelyLessThan accepts.
 *
 * The Solidity version reverts (`MathInvalidRelativeDifference`) above
 * SCALE_FACTOR, and AssemblyScript gives this module no error channel to
 * mirror that with. So the check is exported instead: callers must ask before
 * pricing a request, and degrade to "no upkeep" — see decide.affordableRequests.
 *
 * `AMM.exit` rejects a tolerance above SCALE_FACTOR, so a stored request should
 * never carry one; this is the guard for the day that stops being true.
 */
export function isValidRelativeDifference(difference: BigInt): bool {
  return difference <= SCALE_FACTOR
}

/**
 * Math.isRelativelyLessThan:
 *
 *     a * SCALE_FACTOR < b * (SCALE_FACTOR - difference)
 *
 * In the redemption path this decides whether a queued request is closed at
 * zero ETH because the batch's final price fell more than the user's tolerance
 * below the price they queued at. Getting the direction or the strictness wrong
 * silently mis-prices every request in a falling market, so the comparison is
 * transcribed rather than reformulated.
 *
 * Callers MUST gate on isValidRelativeDifference first. `false` here means
 * "not slippage-closed", i.e. the request costs full price — the opposite of a
 * safe degradation — so an out-of-range difference must never reach this far.
 */
export function isRelativelyLessThan(a: BigInt, b: BigInt, difference: BigInt): bool {
  if (!isValidRelativeDifference(difference)) {
    return false
  }
  return a.times(SCALE_FACTOR) < b.times(SCALE_FACTOR.minus(difference))
}
