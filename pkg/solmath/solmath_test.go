package solmath_test

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"testing"

	"github.com/everstrat-xyz/keepers/pkg/solmath"
)

type fixtures struct {
	ConvertAssets []struct {
		Amount string `json:"amount"`
		Price  string `json:"price"`
		Want   string `json:"want"`
	} `json:"convertAssets"`
	IsRelativelyLessThan []struct {
		A          string `json:"a"`
		B          string `json:"b"`
		Difference string `json:"difference"`
		Want       bool   `json:"want"`
	} `json:"isRelativelyLessThan"`
}

func loadFixtures(t *testing.T) fixtures {
	t.Helper()
	raw, err := os.ReadFile("testdata/fixtures.json")
	if err != nil {
		t.Fatalf("reading fixtures: %v", err)
	}
	var f fixtures
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parsing fixtures: %v", err)
	}
	if len(f.ConvertAssets) == 0 || len(f.IsRelativelyLessThan) == 0 {
		t.Fatal("fixtures are empty")
	}
	return f
}

func mustInt(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("parsing %q as big.Int", s)
	}
	return v
}

// TestConvertAssetsMatchesSolidity compares against values evaluated by chisel
// (Foundry's Solidity REPL) via scripts/gen-solmath-fixtures.sh, so truncating
// integer division is checked against a real uint256 evaluator rather than
// against Go's own arithmetic.
func TestConvertAssetsMatchesSolidity(t *testing.T) {
	for _, tt := range loadFixtures(t).ConvertAssets {
		t.Run(tt.Amount+"x"+tt.Price, func(t *testing.T) {
			got := solmath.ConvertAssets(mustInt(t, tt.Amount), mustInt(t, tt.Price))
			if want := mustInt(t, tt.Want); got.Cmp(want) != 0 {
				t.Errorf("ConvertAssets(%s, %s) = %s, want %s", tt.Amount, tt.Price, got, want)
			}
		})
	}
}

// TestIsRelativelyLessThanMatchesSolidity covers the tolerance boundary in both
// directions: at exactly the tolerance the comparison is false, one wei past it
// is true. Getting that strictness backwards would close every borderline
// redemption at zero ETH.
func TestIsRelativelyLessThanMatchesSolidity(t *testing.T) {
	for _, tt := range loadFixtures(t).IsRelativelyLessThan {
		t.Run(tt.A+"/"+tt.B+"/"+tt.Difference, func(t *testing.T) {
			got, err := solmath.IsRelativelyLessThan(
				mustInt(t, tt.A), mustInt(t, tt.B), mustInt(t, tt.Difference))
			if err != nil {
				t.Fatalf("IsRelativelyLessThan() error = %v", err)
			}
			if got != tt.Want {
				t.Errorf("IsRelativelyLessThan(%s, %s, %s) = %v, want %v",
					tt.A, tt.B, tt.Difference, got, tt.Want)
			}
		})
	}
}

func TestIsRelativelyLessThanRejectsOversizeDifference(t *testing.T) {
	over := new(big.Int).Add(solmath.ScaleFactor, big.NewInt(1))
	_, err := solmath.IsRelativelyLessThan(big.NewInt(1), big.NewInt(1), over)
	if !errors.Is(err, solmath.ErrInvalidRelativeDifference) {
		t.Errorf("error = %v, want %v", err, solmath.ErrInvalidRelativeDifference)
	}

	// Exactly SCALE_FACTOR is allowed by the contract.
	if _, err := solmath.IsRelativelyLessThan(big.NewInt(1), big.NewInt(1), solmath.ScaleFactor); err != nil {
		t.Errorf("difference == SCALE_FACTOR error = %v, want nil", err)
	}
}

func TestConvertAssetsInverse(t *testing.T) {
	// convertAssetsInverse is the exact inverse only when no truncation occurs.
	oneETH := solmath.NormalizationFactor
	got := solmath.ConvertAssetsInverse(big.NewInt(3), oneETH)
	if got.Cmp(big.NewInt(3)) != 0 {
		t.Errorf("ConvertAssetsInverse(3, 1e18) = %s, want 3", got)
	}
}

// TestConvertAssetsDoesNotMutateInputs guards the big.Int aliasing trap: these
// helpers run inside a per-request loop over values read from chain, and an
// in-place mutation would silently corrupt later iterations.
func TestConvertAssetsDoesNotMutateInputs(t *testing.T) {
	amount := mustInt(t, "7000000000000000000")
	price := mustInt(t, "123456789012345678")
	amountCopy := new(big.Int).Set(amount)
	priceCopy := new(big.Int).Set(price)

	solmath.ConvertAssets(amount, price)

	if amount.Cmp(amountCopy) != 0 || price.Cmp(priceCopy) != 0 {
		t.Errorf("ConvertAssets mutated its inputs: amount %s (want %s), price %s (want %s)",
			amount, amountCopy, price, priceCopy)
	}

	a, b, d := mustInt(t, "950000000000000000"), mustInt(t, "1000000000000000000"), mustInt(t, "50000000000000000")
	aCopy, bCopy, dCopy := new(big.Int).Set(a), new(big.Int).Set(b), new(big.Int).Set(d)
	if _, err := solmath.IsRelativelyLessThan(a, b, d); err != nil {
		t.Fatal(err)
	}
	if a.Cmp(aCopy) != 0 || b.Cmp(bCopy) != 0 || d.Cmp(dCopy) != 0 {
		t.Error("IsRelativelyLessThan mutated its inputs")
	}
}
