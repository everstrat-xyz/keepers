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

/** Math.NORMALIZATION_FACTOR (1e18). */
export const NORMALIZATION_FACTOR = 10n ** 18n;

/** Math.SCALE_FACTOR (1e18) — the "100%" denominator for relative comparisons. */
export const SCALE_FACTOR = 10n ** 18n;

/** Math.MathInvalidRelativeDifference. */
export class InvalidRelativeDifference extends Error {
  constructor() {
    super("solmath: relative difference exceeds SCALE_FACTOR");
  }
}

/** Math.convertAssets: `(amount * price) / 1e18`. */
export function convertAssets(normalizedAmount: bigint, normalizedPrice: bigint): bigint {
  return (normalizedAmount * normalizedPrice) / NORMALIZATION_FACTOR;
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
 * The Solidity version reverts when difference > SCALE_FACTOR; this throws
 * instead, because off-chain a bad tolerance read should degrade to "no
 * upkeep", not to a wrong answer.
 */
export function isRelativelyLessThan(a: bigint, b: bigint, difference: bigint): boolean {
  if (difference > SCALE_FACTOR) {
    throw new InvalidRelativeDifference();
  }
  return a * SCALE_FACTOR < b * (SCALE_FACTOR - difference);
}
