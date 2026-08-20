package evmread_test

import (
	"math/big"
	"testing"

	"github.com/everstrat-xyz/keepers/pkg/evmread"
)

// TestUint64AcceptsBothABIShapes pins the reason this converter is lenient
// about its input type.
//
// go-ethereum unpacks a Solidity uint256 into *big.Int but a declared uint64
// into a native uint64, and the keepers read both off the same receiver in one
// multicall — CHAIN_SELECTOR and lastSequence are uint64, the executor knobs
// are uint256. Rejecting either shape here just moves the type switch out to
// every read site, which is where it used to live.
func TestUint64AcceptsBothABIShapes(t *testing.T) {
	fromBig, err := evmread.Uint64(big.NewInt(42), "field")
	if err != nil {
		t.Fatalf("Uint64(*big.Int) returned an error: %v", err)
	}
	if fromBig != 42 {
		t.Errorf("Uint64(*big.Int) = %d, want 42", fromBig)
	}

	fromNative, err := evmread.Uint64(uint64(42), "field")
	if err != nil {
		t.Fatalf("Uint64(uint64) returned an error: %v", err)
	}
	if fromNative != 42 {
		t.Errorf("Uint64(uint64) = %d, want 42", fromNative)
	}
}

func TestUint64RejectsOverflowAndWrongTypes(t *testing.T) {
	// A uint256 that does not fit means the read is wrong, not that the chain
	// is exotic — truncating it silently would corrupt a batch id or timestamp.
	overflow := new(big.Int).Lsh(big.NewInt(1), 64)
	if _, err := evmread.Uint64(overflow, "field"); err == nil {
		t.Error("Uint64(2^64) = nil error, want an overflow error")
	}
	if _, err := evmread.Uint64("0x2a", "field"); err == nil {
		t.Error("Uint64(string) = nil error, want a type error")
	}
}

// TestParseBlockTagDefaultsToFinalized guards the setting that decides whether
// DON consensus can succeed at all: nodes must observe the same block, and
// "latest" differs per node by construction. Anything unrecognised has to
// degrade to finalized rather than to the unsafe option.
func TestParseBlockTagDefaultsToFinalized(t *testing.T) {
	if got := evmread.ParseBlockTag("latest"); got != evmread.BlockLatest {
		t.Errorf(`ParseBlockTag("latest") = %q, want %q`, got, evmread.BlockLatest)
	}
	for _, in := range []string{"", "finalized", "Latest", "LATEST", "safe", "pending", "12345"} {
		if got := evmread.ParseBlockTag(in); got != evmread.BlockFinalized {
			t.Errorf("ParseBlockTag(%q) = %q, want %q", in, got, evmread.BlockFinalized)
		}
	}
}
