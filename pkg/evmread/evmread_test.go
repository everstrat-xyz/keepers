package evmread_test

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/everstrat-xyz/keepers/pkg/evmread"
)

// The Single* family exists because a multicall sub-call can return any shape
// go-ethereum unpacks, and the reader asserts the shape it expects. Both
// halves matter: a wrong arity or a wrong type means the ABI and the reader
// disagree about the view — a wiring bug that would otherwise surface as a
// silent zero value flowing into a decision.

func okResult(v any) evmread.SubResult {
	return evmread.SubResult{Success: true, Values: []any{v}}
}

// arityFailures are the shapes every Single* converter must reject: nothing
// returned (a reverted sub-call keeps Values empty) and more than one value.
func arityFailures(t *testing.T, call func(evmread.SubResult) error) {
	t.Helper()
	for name, r := range map[string]evmread.SubResult{
		"no values":  {Success: true},
		"two values": {Success: true, Values: []any{big.NewInt(1), big.NewInt(2)}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(r); err == nil {
				t.Error("accepted a result that is not a single value")
			}
		})
	}
}

// fieldNamed checks the error carries the field name, per the repo convention
// that an error names the field — a multicall failure without one points at
// sub-call index N, which is useless.
func fieldNamed(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), field) {
		t.Errorf("error = %v, want it to name the field %q", err, field)
	}
}

func TestSingleUint64(t *testing.T) {
	got, err := evmread.SingleUint64(okResult(new(big.Int).SetUint64(25)), "cursor")
	if err != nil || got != 25 {
		t.Errorf("SingleUint64() = %d, %v; want 25, nil", got, err)
	}

	// uint256 values that do not fit uint64 must be refused, not truncated:
	// batch ids and indices are uint64-shaped in this protocol, so an
	// overflow means the read is pointed at the wrong thing.
	_, err = evmread.SingleUint64(okResult(new(big.Int).Lsh(big.NewInt(1), 64)), "cursor")
	fieldNamed(t, err, "cursor")

	_, err = evmread.SingleUint64(okResult(true), "cursor")
	fieldNamed(t, err, "cursor")

	arityFailures(t, func(r evmread.SubResult) error {
		_, err := evmread.SingleUint64(r, "cursor")
		return err
	})
}

func TestSingleBigInt(t *testing.T) {
	want := new(big.Int).Lsh(big.NewInt(1), 200) // full width, far past uint64
	got, err := evmread.SingleBigInt(okResult(want), "controllerBalance")
	if err != nil || got.Cmp(want) != 0 {
		t.Errorf("SingleBigInt() = %v, %v; want %v, nil", got, err, want)
	}

	_, err = evmread.SingleBigInt(okResult(false), "controllerBalance")
	fieldNamed(t, err, "controllerBalance")

	arityFailures(t, func(r evmread.SubResult) error {
		_, err := evmread.SingleBigInt(r, "controllerBalance")
		return err
	})
}

func TestSingleBool(t *testing.T) {
	got, err := evmread.SingleBool(okResult(true), "receiver.paused")
	if err != nil || !got {
		t.Errorf("SingleBool() = %v, %v; want true, nil", got, err)
	}

	// A paused() read that comes back as an integer is an ABI mismatch, not
	// a pause state — refuse it rather than guess.
	_, err = evmread.SingleBool(okResult(big.NewInt(1)), "receiver.paused")
	fieldNamed(t, err, "receiver.paused")

	arityFailures(t, func(r evmread.SubResult) error {
		_, err := evmread.SingleBool(r, "receiver.paused")
		return err
	})
}

func TestSingleAddress(t *testing.T) {
	want := common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11")
	got, err := evmread.SingleAddress(okResult(want), "exitQueue")
	if err != nil || got != want {
		t.Errorf("SingleAddress() = %s, %v; want %s, nil", got.Hex(), err, want.Hex())
	}

	_, err = evmread.SingleAddress(okResult("0xcA11bde05977b3631167028862bE2a173976CA11"), "exitQueue")
	fieldNamed(t, err, "exitQueue")

	arityFailures(t, func(r evmread.SubResult) error {
		_, err := evmread.SingleAddress(r, "exitQueue")
		return err
	})
}
