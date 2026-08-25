package chains_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/everstrat-xyz/keepers/pkg/chains"
)

func validSepoliaConfig() chains.Config {
	return chains.Config{
		ChainName:           "ethereum-testnet-sepolia",
		ChainSelector:       "16015286601757825753",
		RegistryAddress:     "0x000000000000000000000000000000000000dEaD",
		ShadowMode:          true,
		MaxReportAgeSeconds: 3600,
	}
}

const validReceiver = "0x000000000000000000000000000000000000bEEF"

func TestChainConstants(t *testing.T) {
	// Selectors are the CCIP chain ids the (now-retired) CRE envelope
	// validated against; W4's config validation still requires the pair, and
	// they remain the canonical identifiers for these chains. Documented in
	// README.md.
	if chains.Sepolia.Selector != 16015286601757825753 {
		t.Errorf("Sepolia selector = %d", chains.Sepolia.Selector)
	}
	if chains.Mainnet.Selector != 5009297550715157269 {
		t.Errorf("Mainnet selector = %d", chains.Mainnet.Selector)
	}
	if got, want := chains.Sepolia.Forwarder.Hex(), "0xF8344CFd5c43616a4366C34E3EEE75af79a74482"; got != want {
		t.Errorf("Sepolia forwarder = %s, want %s", got, want)
	}
	if got, want := chains.Mainnet.Forwarder.Hex(), "0x0b93082D9b3C7C97fAcd250082899BAcf3af3885"; got != want {
		t.Errorf("Mainnet forwarder = %s, want %s", got, want)
	}
	if !chains.Sepolia.Testnet || chains.Mainnet.Testnet {
		t.Error("Testnet flags are wrong")
	}
	if chains.All[0] != chains.Sepolia {
		t.Error("All[0] should be Sepolia — the cutover happens there first")
	}

	// The stale mock forwarder some older CRE samples cite. If it ever shows
	// up here, simulation is pointed at an address the directory does not list.
	stale := common.HexToAddress("0x15fC6ae953E024d975e77382eEeC56A9101f9F88")
	for _, c := range chains.All {
		if c.MockForwarder == stale {
			t.Errorf("%s MockForwarder is the stale sample address", c.Name)
		}
		if c.Forwarder == (common.Address{}) || c.MockForwarder == (common.Address{}) {
			t.Errorf("%s has a zero forwarder", c.Name)
		}
	}
}

func TestLookups(t *testing.T) {
	got, err := chains.ByName("ethereum-testnet-sepolia")
	if err != nil || got != chains.Sepolia {
		t.Errorf("ByName(sepolia) = %+v, %v", got, err)
	}
	if _, err := chains.ByName("ethereum-testnet-goerli"); !errors.Is(err, chains.ErrUnknownChain) {
		t.Errorf("ByName(goerli) error = %v, want %v", err, chains.ErrUnknownChain)
	}

	got, err = chains.BySelector(5009297550715157269)
	if err != nil || got != chains.Mainnet {
		t.Errorf("BySelector(mainnet) = %+v, %v", got, err)
	}
	if _, err := chains.BySelector(1); !errors.Is(err, chains.ErrUnknownChain) {
		t.Errorf("BySelector(1) error = %v, want %v", err, chains.ErrUnknownChain)
	}
}

func TestResolveValidConfig(t *testing.T) {
	got, err := chains.Resolve(validSepoliaConfig(), validReceiver)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Chain != chains.Sepolia {
		t.Errorf("Chain = %+v, want Sepolia", got.Chain)
	}
	if got.Registry != common.HexToAddress("0x000000000000000000000000000000000000dEaD") {
		t.Errorf("Registry = %s", got.Registry)
	}
	if got.Receiver != common.HexToAddress(validReceiver) {
		t.Errorf("Receiver = %s", got.Receiver)
	}
	if !got.ShadowMode {
		t.Error("ShadowMode = false, want true")
	}
	if got.MaxReportAgeSeconds != 3600 {
		t.Errorf("MaxReportAgeSeconds = %d, want 3600", got.MaxReportAgeSeconds)
	}
}

func TestResolveRejectsBadConfig(t *testing.T) {
	// receiver overrides the default when non-nil, so "" is expressible.
	ptr := func(s string) *string { return &s }

	tests := []struct {
		name     string
		mut      func(*chains.Config)
		receiver *string
		want     error
	}{
		{
			name: "unknown chain name",
			mut:  func(c *chains.Config) { c.ChainName = "ethereum-testnet-holesky" },
			want: chains.ErrUnknownChain,
		},
		{
			// Copying a config between environments and updating only one of
			// the two fields is the realistic way this happens.
			name: "selector belongs to a different chain",
			mut:  func(c *chains.Config) { c.ChainSelector = "5009297550715157269" },
			want: chains.ErrSelectorMismatch,
		},
		{
			name: "selector overflows uint64",
			mut:  func(c *chains.Config) { c.ChainSelector = "184467440737095516150" },
		},
		{
			name: "selector missing",
			mut:  func(c *chains.Config) { c.ChainSelector = "" },
		},
		{
			// The placeholder the scaffold configs ship with.
			name: "registry left at the zero address",
			mut:  func(c *chains.Config) { c.RegistryAddress = "0x0000000000000000000000000000000000000000" },
			want: chains.ErrZeroAddress,
		},
		{
			name: "registry missing",
			mut:  func(c *chains.Config) { c.RegistryAddress = "" },
			want: chains.ErrMissingAddress,
		},
		{
			name: "registry not an address",
			mut:  func(c *chains.Config) { c.RegistryAddress = "0xdead" },
			want: chains.ErrInvalidAddress,
		},
		{
			name: "registry checksum broken",
			mut:  func(c *chains.Config) { c.RegistryAddress = "0x000000000000000000000000000000000000dEad" },
			want: chains.ErrInvalidChecksum,
		},
		{
			name: "maxReportAgeSeconds zero",
			mut:  func(c *chains.Config) { c.MaxReportAgeSeconds = 0 },
			want: chains.ErrZeroMaxReportAge,
		},
		{
			name: "maxReportAgeSeconds beyond the sanity ceiling",
			mut:  func(c *chains.Config) { c.MaxReportAgeSeconds = chains.MaxReportAgeCeiling + 1 },
			want: chains.ErrMaxReportAgeLarge,
		},
		{
			name:     "receiver left at the zero address",
			mut:      func(*chains.Config) {},
			receiver: ptr("0x0000000000000000000000000000000000000000"),
			want:     chains.ErrZeroAddress,
		},
		{
			name:     "receiver missing",
			mut:      func(*chains.Config) {},
			receiver: ptr(""),
			want:     chains.ErrMissingAddress,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validSepoliaConfig()
			tt.mut(&cfg)

			receiver := validReceiver
			if tt.receiver != nil {
				receiver = *tt.receiver
			}

			_, err := chains.Resolve(cfg, receiver)
			if err == nil {
				t.Fatal("Resolve() succeeded, want error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("Resolve() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestParseAddressAcceptsUnchecksummedCase(t *testing.T) {
	// All-lower and all-upper carry no checksum to verify, so both must pass.
	for _, s := range []string{
		"0x000000000000000000000000000000000000dead",
		"0X000000000000000000000000000000000000DEAD",
	} {
		got, err := chains.ParseAddress("registryAddress", s)
		if err != nil {
			t.Errorf("ParseAddress(%q) error = %v", s, err)
			continue
		}
		if got != common.HexToAddress("0x000000000000000000000000000000000000dEaD") {
			t.Errorf("ParseAddress(%q) = %s", s, got)
		}
	}
}

// TestScaffoldConfigsAreShapedCorrectly reads the real workflow configs. They
// still carry zero-address placeholders (deployment happens at cutover), so
// this asserts the parts that must already be right — chain name and selector
// agreeing, and a report age within bounds — rather than requiring live
// addresses.
func TestScaffoldConfigsAreShapedCorrectly(t *testing.T) {
	root := filepath.Join("..", "..")
	configs := []string{
		"freeze-watch/config.staging.json",
		"freeze-watch/config.production.json",
	}

	for _, rel := range configs {
		t.Run(rel, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				t.Fatalf("reading config: %v", err)
			}
			var cfg chains.Config
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatalf("parsing config: %v", err)
			}

			chain, err := chains.ByName(cfg.ChainName)
			if err != nil {
				t.Fatalf("chainName: %v", err)
			}
			selector, err := chains.ParseSelector(cfg.ChainSelector)
			if err != nil {
				t.Fatalf("chainSelector: %v", err)
			}
			if selector != chain.Selector {
				t.Errorf("chainSelector %d does not match %s (%d)", selector, chain.Name, chain.Selector)
			}
			if cfg.MaxReportAgeSeconds == 0 || cfg.MaxReportAgeSeconds > chains.MaxReportAgeCeiling {
				t.Errorf("maxReportAgeSeconds = %d, want 1..%d", cfg.MaxReportAgeSeconds, chains.MaxReportAgeCeiling)
			}
			if !cfg.ShadowMode {
				t.Error("shadowMode = false; W4 must stay read-only")
			}
		})
	}
}
