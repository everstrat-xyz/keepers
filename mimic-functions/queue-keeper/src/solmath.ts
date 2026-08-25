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
 * The Solidity version reverts when difference > SCALE_FACTOR. Off-chain a bad
 * tolerance read should degrade to "no upkeep", not to a wrong answer — so this
 * returns false (not-slippage-closed) only after the caller has had a chance
 * to see the anomaly; the executor re-derives the truth on-chain regardless.
 */
export function isRelativelyLessThan(a: BigInt, b: BigInt, difference: BigInt): bool {
  if (difference > SCALE_FACTOR) {
    return false
  }
  return a.times(SCALE_FACTOR) < b.times(SCALE_FACTOR.minus(difference))
}
