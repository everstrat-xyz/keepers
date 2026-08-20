// Package solmath mirrors the arithmetic in the contracts' `Math` library.
//
// The keeper workflows have to reproduce on-chain affordability and NAV
// arithmetic off-chain, and "close enough" is not good enough: a workflow that
// rounds differently from the contract proposes work the contract then refuses,
// producing a revert storm that looks exactly like a broken keeper.
//
// Every function here is a literal transcription of its Solidity counterpart,
// including integer-division truncation. `big.Int` division truncates toward
// zero and all inputs are non-negative, so it matches EVM `/` exactly.
package solmath

import (
	"errors"
	"math/big"
)

// Decimals is Math.DECIMALS_NORMALIZED.
const Decimals = 18

var (
	// NormalizationFactor is Math.NORMALIZATION_FACTOR (1e18).
	NormalizationFactor = exp10(Decimals)
	// ScaleFactor is Math.SCALE_FACTOR (1e18) — the "100%" denominator for
	// relative comparisons.
	ScaleFactor = exp10(18)
)

// ErrInvalidRelativeDifference mirrors Math.MathInvalidRelativeDifference.
var ErrInvalidRelativeDifference = errors.New("solmath: relative difference exceeds SCALE_FACTOR")

func exp10(n int64) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil)
}

// ConvertAssets is Math.convertAssets: `(amount * price) / 1e18`.
//
// Used to price a redemption: `tokensToBurn` EVE at `finalEvePrice` gives the
// ETH the Controller must send.
func ConvertAssets(normalizedAmount, normalizedPrice *big.Int) *big.Int {
	out := new(big.Int).Mul(normalizedAmount, normalizedPrice)
	return out.Div(out, NormalizationFactor)
}

// ConvertAssetsInverse is Math.convertAssetsInverse: `(amount * 1e18) / price`.
//
// Panics on a zero price, matching the EVM's division-by-zero revert; callers
// that can see a zero price must check first.
func ConvertAssetsInverse(normalizedAmount, normalizedPrice *big.Int) *big.Int {
	out := new(big.Int).Mul(normalizedAmount, NormalizationFactor)
	return out.Div(out, normalizedPrice)
}

// IsRelativelyLessThan is Math.isRelativelyLessThan:
//
//	a * SCALE_FACTOR < b * (SCALE_FACTOR - difference)
//
// In the redemption path this decides whether a queued request is closed at
// zero ETH because the batch's final price fell more than the user's tolerance
// below the price they queued at. Getting the direction or the strictness wrong
// silently mis-prices every request in a falling market, so the comparison is
// transcribed rather than reformulated.
//
// The Solidity version reverts when difference > SCALE_FACTOR; this returns an
// error instead, because off-chain a bad tolerance read should degrade to "no
// upkeep", not to a wrong answer.
func IsRelativelyLessThan(a, b, difference *big.Int) (bool, error) {
	if difference.Cmp(ScaleFactor) > 0 {
		return false, ErrInvalidRelativeDifference
	}
	left := new(big.Int).Mul(a, ScaleFactor)
	right := new(big.Int).Sub(ScaleFactor, difference)
	right.Mul(b, right)
	return left.Cmp(right) < 0, nil
}
