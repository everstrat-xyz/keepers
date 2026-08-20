package registry_test

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/everstrat-xyz/keepers/pkg/registry"
)

// TestKeysMatchTheirPreimages recomputes every hardcoded identifier from its
// source string. The constants exist so the WASM workflows do not ship a Keccak
// implementation; this test is what makes hardcoding them safe.
func TestKeysMatchTheirPreimages(t *testing.T) {
	for key, name := range registry.Preimages {
		t.Run(name, func(t *testing.T) {
			if want := crypto.Keccak256Hash([]byte(name)); key != want {
				t.Errorf("keccak256(%q) = %s, hardcoded %s", name, want, key)
			}
		})
	}
}

// TestPreimagesCoverEveryExportedKey guards against adding a constant without
// registering it, which would leave Name() rendering raw hex and this file's
// verification silently skipping it.
func TestPreimagesCoverEveryExportedKey(t *testing.T) {
	exported := map[string]common.Hash{
		"KeyController":             registry.KeyController,
		"KeyAMM":                    registry.KeyAMM,
		"KeyStrategyManager":        registry.KeyStrategyManager,
		"KeyExitQueue":              registry.KeyExitQueue,
		"KeyOracle":                 registry.KeyOracle,
		"KeyEVE":                    registry.KeyEVE,
		"KeyConverter":              registry.KeyConverter,
		"KeyQueueKeeperExecutor":    registry.KeyQueueKeeperExecutor,
		"KeyStrategyKeeperExecutor": registry.KeyStrategyKeeperExecutor,
		"KeyWhitelist":              registry.KeyWhitelist,
		"RoleAdmin":                 registry.RoleAdmin,
		"RoleSecurity":              registry.RoleSecurity,
		"RoleKeeper":                registry.RoleKeeper,
	}

	if len(registry.Preimages) != len(exported) {
		t.Errorf("Preimages has %d entries, %d identifiers are exported", len(registry.Preimages), len(exported))
	}
	for varName, h := range exported {
		if _, ok := registry.Preimages[h]; !ok {
			t.Errorf("%s (%s) is missing from Preimages", varName, h)
		}
	}
}

// TestKeysAreDistinct catches a copy-paste that would silently resolve two
// different contracts to the same Registry slot.
func TestKeysAreDistinct(t *testing.T) {
	seen := map[common.Hash]bool{}
	for key, name := range registry.Preimages {
		if seen[key] {
			t.Errorf("%s duplicates an earlier identifier (%s)", name, key)
		}
		seen[key] = true
	}
}

func TestName(t *testing.T) {
	if got := registry.Name(registry.KeyController); got != "CONTROLLER" {
		t.Errorf("Name(KeyController) = %q, want CONTROLLER", got)
	}
	unknown := common.HexToHash("0xdead")
	if got := registry.Name(unknown); got != unknown.Hex() {
		t.Errorf("Name(unknown) = %q, want %s", got, unknown.Hex())
	}
}
