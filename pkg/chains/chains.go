// Package chains holds the per-chain constants the keeper workflows need and
// validates the deployment addresses that arrive from `config.<target>.json`.
//
// # What lives where
//
//   - Chain-level facts (CRE chain name, CCIP chain selector, KeystoneForwarder
//     addresses) are compiled in. They are public protocol constants, identical
//     for every EverStrat deployment on that chain, and pinning them here means
//     a typo in a config file fails a unit test instead of a cutover.
//   - Deployment-level facts (Registry proxy, receiver addresses) come from
//     workflow config, because they differ per environment and change on
//     redeploy.
//
// # No secrets
//
// Everything here is public on-chain data. Nothing in this package may be read
// from `secrets.yaml` or `runtime.GetSecret()` — see secrets.yaml for what
// actually belongs there.
package chains

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// Chain is a supported destination chain.
type Chain struct {
	// Name is the CRE chain name, as reported by `cre workflow supported-chains`
	// and used in project.yaml `rpcs[].chain-name`.
	Name string
	// Selector is the CCIP chain selector. It must equal the receiver's
	// immutable CHAIN_SELECTOR, and is what CREReceiverBase checks the Envelope
	// against.
	Selector uint64
	// Forwarder is the production KeystoneForwarder — the only address the
	// receiver accepts `onReport` from.
	Forwarder common.Address
	// MockForwarder is the simulation forwarder. Verify against
	// `cre workflow supported-chains` for your tenant before relying on it:
	// the directory value and a tenant's mock deployment can differ.
	MockForwarder common.Address
	// Testnet marks chains where shadow-mode mistakes are cheap.
	Testnet bool
}

// Supported chains. Addresses re-verified against the CRE Forwarder Directory
// (docs last updated 2026-06-24); re-check before any deploy, since a stale
// forwarder means every report is rejected with InvalidSender.
//
// https://docs.chain.link/cre/guides/workflow/using-evm-client/forwarder-directory-go
var (
	Sepolia = Chain{
		Name:          "ethereum-testnet-sepolia",
		Selector:      16015286601757825753,
		Forwarder:     common.HexToAddress("0xF8344CFd5c43616a4366C34E3EEE75af79a74482"),
		MockForwarder: common.HexToAddress("0xF8344CFd5c43616a4366C34E3EEE75af79a74482"),
		Testnet:       true,
	}

	Mainnet = Chain{
		Name:          "ethereum-mainnet",
		Selector:      5009297550715157269,
		Forwarder:     common.HexToAddress("0x0b93082D9b3C7C97fAcd250082899BAcf3af3885"),
		MockForwarder: common.HexToAddress("0x0b93082D9b3C7C97fAcd250082899BAcf3af3885"),
		Testnet:       false,
	}
)

// All lists every supported chain, Sepolia first — the cutover order.
var All = []Chain{Sepolia, Mainnet}

var (
	ErrUnknownChain      = errors.New("chains: unknown chain")
	ErrSelectorMismatch  = errors.New("chains: chainSelector does not match chainName")
	ErrMissingAddress    = errors.New("chains: address is required")
	ErrZeroAddress       = errors.New("chains: address is the zero address (placeholder not replaced?)")
	ErrInvalidAddress    = errors.New("chains: address is not a 20-byte hex address")
	ErrInvalidChecksum   = errors.New("chains: address has an invalid EIP-55 checksum")
	ErrZeroMaxReportAge  = errors.New("chains: maxReportAgeSeconds must be non-zero")
	ErrMaxReportAgeLarge = errors.New("chains: maxReportAgeSeconds exceeds the sanity ceiling")
)

// MaxReportAgeCeiling is a workflow-side sanity bound on configured report age.
// The contract only rejects zero, but an age measured in days would let a
// long-stalled workflow deliver an observation that no longer describes
// reality, which is precisely the staleness the Envelope exists to prevent.
const MaxReportAgeCeiling = uint64(24 * 60 * 60)

// ByName returns the chain with the given CRE chain name.
func ByName(name string) (Chain, error) {
	for _, c := range All {
		if c.Name == name {
			return c, nil
		}
	}
	return Chain{}, fmt.Errorf("%w: %q (supported: %s)", ErrUnknownChain, name, strings.Join(names(), ", "))
}

// BySelector returns the chain with the given CCIP chain selector.
func BySelector(selector uint64) (Chain, error) {
	for _, c := range All {
		if c.Selector == selector {
			return c, nil
		}
	}
	return Chain{}, fmt.Errorf("%w: selector %d", ErrUnknownChain, selector)
}

func names() []string {
	out := make([]string, len(All))
	for i, c := range All {
		out[i] = c.Name
	}
	return out
}

// Config is the chain-related subset of a workflow's `config.<target>.json`.
// Both W1 and W2 embed these fields; the receiver field differs per workflow
// and is passed to Resolve separately.
type Config struct {
	ChainName           string `json:"chainName"`
	ChainSelector       string `json:"chainSelector"` // decimal string: uint64 exceeds JSON's safe integer range
	RegistryAddress     string `json:"registryAddress"`
	ShadowMode          bool   `json:"shadowMode"`
	MaxReportAgeSeconds uint64 `json:"maxReportAgeSeconds"`
}

// Deployment is a validated binding of a workflow to one chain and one set of
// contracts.
type Deployment struct {
	Chain Chain
	// Registry is the EverStrat Registry proxy. Every other protocol address
	// is resolved from it at runtime via getContractByKey, so only this one
	// needs to be configured.
	Registry common.Address
	// Receiver is the CRE executor this workflow reports to
	// (CREQueueExecutor for W1, CREStrategyExecutor for W2).
	Receiver common.Address
	// ShadowMode suppresses writeReport. Keep it true until the cutover issue
	// binds identity on-chain.
	ShadowMode bool
	// MaxReportAgeSeconds mirrors the receiver's immutable MAX_REPORT_AGE.
	// It is config, so it can drift from the deployed value — read
	// MAX_REPORT_AGE() on-chain before trusting it for staleness decisions.
	MaxReportAgeSeconds uint64
}

// Resolve validates a workflow config against the compiled-in chain table and
// returns the binding to use for the run.
//
// receiverAddress is the workflow's own executor address, read from the
// workflow-specific config field (`queueExecutorAddress` / `strategyExecutorAddress`).
func Resolve(cfg Config, receiverAddress string) (Deployment, error) {
	chain, err := ByName(cfg.ChainName)
	if err != nil {
		return Deployment{}, err
	}

	selector, err := ParseSelector(cfg.ChainSelector)
	if err != nil {
		return Deployment{}, err
	}
	if selector != chain.Selector {
		return Deployment{}, fmt.Errorf("%w: config says %d for %s, expected %d",
			ErrSelectorMismatch, selector, chain.Name, chain.Selector)
	}

	registry, err := ParseAddress("registryAddress", cfg.RegistryAddress)
	if err != nil {
		return Deployment{}, err
	}
	receiver, err := ParseAddress("receiverAddress", receiverAddress)
	if err != nil {
		return Deployment{}, err
	}

	switch {
	case cfg.MaxReportAgeSeconds == 0:
		return Deployment{}, ErrZeroMaxReportAge
	case cfg.MaxReportAgeSeconds > MaxReportAgeCeiling:
		return Deployment{}, fmt.Errorf("%w: %d > %d", ErrMaxReportAgeLarge, cfg.MaxReportAgeSeconds, MaxReportAgeCeiling)
	}

	return Deployment{
		Chain:               chain,
		Registry:            registry,
		Receiver:            receiver,
		ShadowMode:          cfg.ShadowMode,
		MaxReportAgeSeconds: cfg.MaxReportAgeSeconds,
	}, nil
}

// ParseSelector parses a decimal chain-selector string.
//
// Selectors are carried as strings because they exceed 2^53 and would lose
// precision through any JSON parser that decodes numbers as float64.
func ParseSelector(s string) (uint64, error) {
	if s == "" {
		return 0, errors.New("chains: chainSelector is required")
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("chains: parsing chainSelector %q: %w", s, err)
	}
	if v == 0 {
		return 0, errors.New("chains: chainSelector must be non-zero")
	}
	return v, nil
}

// ParseAddress parses and sanity-checks a configured contract address.
//
// The zero address is rejected explicitly: it is the placeholder the scaffold
// configs ship with, and accepting it would turn a forgotten config edit into
// reports aimed at nowhere.
//
// A mixed-case address must carry a valid EIP-55 checksum; an all-lower or
// all-upper address is accepted as-is, since those carry no checksum to verify.
func ParseAddress(field, s string) (common.Address, error) {
	if s == "" {
		return common.Address{}, fmt.Errorf("%w: %s", ErrMissingAddress, field)
	}
	if !common.IsHexAddress(s) {
		return common.Address{}, fmt.Errorf("%w: %s = %q", ErrInvalidAddress, field, s)
	}

	addr := common.HexToAddress(s)
	if addr == (common.Address{}) {
		return common.Address{}, fmt.Errorf("%w: %s", ErrZeroAddress, field)
	}

	if body := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X"); isMixedCase(body) {
		if addr.Hex() != ensure0x(s) {
			return common.Address{}, fmt.Errorf("%w: %s = %q (want %s)", ErrInvalidChecksum, field, s, addr.Hex())
		}
	}
	return addr, nil
}

func isMixedCase(s string) bool {
	var hasUpper, hasLower bool
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'f':
			hasLower = true
		case r >= 'A' && r <= 'F':
			hasUpper = true
		}
	}
	return hasUpper && hasLower
}

func ensure0x(s string) string {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return "0x" + s[2:]
	}
	return "0x" + s
}
