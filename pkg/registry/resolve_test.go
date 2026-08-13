package registry_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	everabi "github.com/everstrat-xyz/keepers/contracts/evm/src/abi"
	"github.com/everstrat-xyz/keepers/pkg/registry"
)

// TestKeysMatchAuth pins every address-book key against the string `Auth.sol`
// hashes. A key that drifts resolves to the zero address on a live deployment,
// which is a deploy-day failure rather than a test-day one.
func TestKeysMatchAuth(t *testing.T) {
	for _, k := range registry.AllKeys {
		t.Run(k.Name, func(t *testing.T) {
			if want := crypto.Keccak256Hash([]byte(k.Name)); k.Hash != want {
				t.Errorf("keccak256(%q) = %s, key has %s", k.Name, want, k.Hash)
			}
		})
	}
}

// TestEveryKeyIsInPreimages keeps Name() able to render the whole book, and
// keeps the constants and the address book from drifting apart.
func TestEveryKeyIsInPreimages(t *testing.T) {
	for _, k := range registry.AllKeys {
		if got := registry.Name(k.Hash); got != k.Name {
			t.Errorf("Name(%s) = %q, want %q", k.Hash, got, k.Name)
		}
	}
}

// TestBoundABIsAreVendoredAndUsable is the point of binding an ABI to a key:
// the mapping is asserted once, here, instead of being re-derived at each call
// site where a mismatch would only surface as a far-away revert.
func TestBoundABIsAreVendoredAndUsable(t *testing.T) {
	// Each key's ABI must actually describe that contract, spot-checked by a
	// method only that contract has.
	probes := map[string]string{
		"CONTROLLER":               "processRequests",
		"EXIT_QUEUE":               "currentBatchId",
		"AMM":                      "freeBalance",
		"STRATEGY_MANAGER":         "strategies",
		"ORACLE":                   "getUsdPrice",
		"QUEUE_KEEPER_EXECUTOR":    "queueUpkeepStatus",
		"STRATEGY_KEEPER_EXECUTOR": "strategyUpkeepStatus",
	}

	for _, k := range registry.AllKeys {
		probe, expected := probes[k.Name]
		if !expected {
			// Keys with no vendored ABI are legal; they carry an address only.
			if k.ABI != "" {
				t.Errorf("%s binds ABI %q but has no probe — add one", k.Name, k.ABI)
			}
			continue
		}
		t.Run(k.Name, func(t *testing.T) {
			if k.ABI == "" {
				t.Fatalf("%s has no ABI bound", k.Name)
			}
			if _, err := everabi.Get(k.ABI); err != nil {
				t.Fatalf("%s binds unvendored ABI %q: %v", k.Name, k.ABI, err)
			}
			if _, err := everabi.Method(k.ABI, probe); err != nil {
				t.Errorf("%s is bound to %s, which has no %s — wrong ABI for this key: %v",
					k.Name, k.ABI, probe, err)
			}
		})
	}
}

// TestSubBindsAddressAndABITogether is the mis-pairing guarantee: a sub-call
// built from a Contract carries that contract's own ABI, so the ExitQueue's
// address can never be sent the Controller's selectors.
func TestSubBindsAddressAndABITogether(t *testing.T) {
	addr := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	c := registry.Contract{Key: registry.ExitQueue, Address: addr}

	sub := c.Sub("currentBatchId")
	if sub.To != addr {
		t.Errorf("To = %s, want %s", sub.To, addr)
	}
	if sub.ABI != everabi.IExitQueue {
		t.Errorf("ABI = %s, want %s", sub.ABI, everabi.IExitQueue)
	}
	if sub.Method != "currentBatchId" {
		t.Errorf("Method = %q", sub.Method)
	}

	// Pausable is inherited rather than declared, so it gets its own builder
	// and must not reach for the contract's own ABI.
	p := c.Paused()
	if p.To != addr || p.ABI != everabi.Pausable || p.Method != "paused" {
		t.Errorf("Paused() = %+v, want the Pausable fragment against %s", p, addr)
	}
}

func TestSubCarriesArgs(t *testing.T) {
	c := registry.Contract{Key: registry.ExitQueue, Address: common.HexToAddress("0x01")}
	sub := c.Sub("batchInfo", 7)
	if len(sub.Args) != 1 || sub.Args[0] != 7 {
		t.Errorf("Args = %v, want [7]", sub.Args)
	}
}

func TestGetErrors(t *testing.T) {
	var empty registry.Protocol

	t.Run("unresolved key", func(t *testing.T) {
		_, err := empty.Get(registry.Controller)
		if !errors.Is(err, registry.ErrNotResolved) {
			t.Errorf("error = %v, want %v", err, registry.ErrNotResolved)
		}
		// The message must say what *was* resolved, or debugging a missing key
		// means re-reading the caller.
		if !strings.Contains(err.Error(), "resolved:") {
			t.Errorf("error %q does not list what was resolved", err)
		}
	})

	t.Run("typed accessor reports the same", func(t *testing.T) {
		if _, err := empty.ExitQueue(); !errors.Is(err, registry.ErrNotResolved) {
			t.Errorf("error = %v, want %v", err, registry.ErrNotResolved)
		}
	})

	t.Run("MustGet degrades instead of panicking", func(t *testing.T) {
		// A panic inside a WASM workflow loses the log line that would explain
		// it, so a miss yields a zero-address Contract instead.
		c := empty.MustGet(registry.Controller)
		if c.Address != (common.Address{}) {
			t.Errorf("Address = %s, want zero", c.Address)
		}
		if c.Key.Name != "CONTROLLER" {
			t.Errorf("Key.Name = %q, want CONTROLLER", c.Key.Name)
		}
	})
}
