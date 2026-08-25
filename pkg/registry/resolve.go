package registry

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	everabi "github.com/everstrat-xyz/keepers/contracts/evm/src/abi"
	"github.com/everstrat-xyz/keepers/pkg/evmread"
)

// This file is the off-chain half of the protocol's address book — the Go
// mirror of `Registry.sol` plus `Auth.sol`'s typed accessors.
//
// # Why this exists
//
// On-chain, no contract stores its peers' addresses: it holds the Registry and
// asks it. That is what survives a redeploy. Off-chain the same rule applies,
// and before this every workflow resolved its own addresses inline — three
// copies of the same multicall, each with a slightly different key set, each
// re-deriving which ABI went with which address.
//
// So: one place that knows the key → address → ABI mapping, resolves the whole
// set in a single chain read, and hands back handles that cannot be
// mis-paired.
//
// # The address/ABI pairing
//
// A raw `(address, abi.Name)` call site can silently pair the ExitQueue's
// address with the Controller's ABI — the selectors simply will not match and
// the call reverts somewhere far from the mistake. A Contract carries both, so
// `c.Sub("currentBatchId")` is either right or does not compile.

// Key identifies one registered contract: its Registry key, a readable name,
// and the ABI that describes it.
//
// The ABI binding is the piece that has no on-chain equivalent, and the reason
// this is more than a constant list.
type Key struct {
	Hash common.Hash
	Name string
	ABI  everabi.Name
}

// The registered protocol contracts, mirroring `Auth`'s key constants.
//
// Keys without a vendored ABI (EVE, Converter, Whitelist) are still resolvable
// — a workflow may need the address to compare against — but calling through
// them fails loudly rather than guessing.
var (
	Controller             = Key{KeyController, "CONTROLLER", everabi.IController}
	ExitQueue              = Key{KeyExitQueue, "EXIT_QUEUE", everabi.IExitQueue}
	AMM                    = Key{KeyAMM, "AMM", everabi.IAMM}
	StrategyManager        = Key{KeyStrategyManager, "STRATEGY_MANAGER", everabi.IStrategyManager}
	Oracle                 = Key{KeyOracle, "ORACLE", everabi.IOracle}
	QueueKeeperExecutor    = Key{KeyQueueKeeperExecutor, "QUEUE_KEEPER_EXECUTOR", everabi.IQueueKeeperExecutor}
	StrategyKeeperExecutor = Key{KeyStrategyKeeperExecutor, "STRATEGY_KEEPER_EXECUTOR", everabi.IStrategyKeeperExecutor}
	EVE                    = Key{KeyEVE, "EVE", ""}
	Converter              = Key{KeyConverter, "CONVERTER", ""}
	Whitelist              = Key{KeyWhitelist, "WHITELIST", ""}
)

// AllKeys lists every registered contract, for tests and for tooling that wants
// to dump the whole address book.
var AllKeys = []Key{
	Controller, ExitQueue, AMM, StrategyManager, Oracle,
	QueueKeeperExecutor, StrategyKeeperExecutor, EVE, Converter, Whitelist,
}

// Contract is a resolved address bound to the ABI that describes it.
type Contract struct {
	Key     Key
	Address common.Address
}

// Sub builds a Multicall3 sub-call against this contract, filling in both the
// address and the matching ABI.
func (c Contract) Sub(method string, args ...any) evmread.SubCall {
	return evmread.SubCall{To: c.Address, ABI: c.Key.ABI, Method: method, Args: args}
}

// Paused builds a sub-call for OpenZeppelin's `paused()`.
//
// Separate from Sub because Pausable is inherited rather than declared on the
// EverStrat interfaces, so it is not in the contract's own ABI.
func (c Contract) Paused() evmread.SubCall {
	return evmread.SubCall{To: c.Address, ABI: everabi.Pausable, Method: "paused"}
}

// String renders the contract for logs.
func (c Contract) String() string {
	return fmt.Sprintf("%s(%s)", c.Key.Name, c.Address.Hex())
}

// Protocol is the resolved address book for one tick.
//
// Resolve it once and pass it down; it holds no live connection and is safe to
// copy.
type Protocol struct {
	// Address is the Registry itself — the one address that must be
	// configured, because everything else is derived from it.
	Address   common.Address
	contracts map[common.Hash]Contract
}

var (
	// ErrNotResolved means a key was asked for that Resolve was not given.
	ErrNotResolved = fmt.Errorf("registry: contract was not resolved this tick")
	// ErrUnregistered means the Registry returned the zero address, i.e. the
	// contract is not registered on this deployment.
	ErrUnregistered = fmt.Errorf("registry: contract is not registered")
	// ErrNoABI means the key has no vendored ABI, so it cannot be called
	// through — only its address is available.
	ErrNoABI = fmt.Errorf("registry: no ABI is vendored for this contract")
)

// Resolve reads the requested keys from the Registry in a single chain read.
//
// One read regardless of how many keys are asked for, which matters against
// CRE's per-execution budget — see docs/READ_BUDGET.md. The caller is expected
// to have taken that read from its evmread.Budget already. Use ResolveWith to
// fold the caller's own independent reads into the same one.
//
// A key that resolves to the zero address fails the whole call: it means the
// deployment is incomplete, and every later read against that address would
// fail somewhere less obvious.
func Resolve(c *evmread.Caller, registryAddress common.Address, keys ...Key) (Protocol, error) {
	p, _, err := ResolveWith(c, registryAddress, keys, nil)
	return p, err
}

// ResolveWith resolves the address book in the *same* chain read as the
// caller's own sub-calls, returning their results alongside.
//
// Multicall3 does not care that the sub-calls target different contracts, and
// against a budget of 15 reads per execution a spare round trip is expensive —
// so any read that does not depend on the resolved addresses should ride along
// here rather than paying for a round of its own.
//
// The returned slice lines up with `extra`, index for index.
func ResolveWith(
	c *evmread.Caller,
	registryAddress common.Address,
	keys []Key,
	extra []evmread.SubCall,
) (Protocol, []evmread.SubResult, error) {
	p := Protocol{Address: registryAddress, contracts: map[common.Hash]Contract{}}
	if len(keys) == 0 && len(extra) == 0 {
		return p, nil, nil
	}
	if registryAddress == (common.Address{}) {
		return Protocol{}, nil, fmt.Errorf("registry: address is the zero address")
	}

	calls := make([]evmread.SubCall, 0, len(keys)+len(extra))
	for _, k := range keys {
		calls = append(calls, evmread.SubCall{
			To: registryAddress, ABI: everabi.IRegistry,
			Method: "getContractByKey", Args: []any{k.Hash},
		})
	}
	calls = append(calls, extra...)

	results, err := c.Aggregate(calls, false).Await()
	if err != nil {
		return Protocol{}, nil, fmt.Errorf("registry: resolving %s from %s: %w",
			names(keys), registryAddress, err)
	}

	for i, k := range keys {
		if len(results[i].Values) != 1 {
			return Protocol{}, nil, fmt.Errorf("registry: %s lookup returned %d values, want 1",
				k.Name, len(results[i].Values))
		}
		addr, err := evmread.Address(results[i].Values[0], k.Name)
		if err != nil {
			return Protocol{}, nil, err
		}
		if addr == (common.Address{}) {
			return Protocol{}, nil, fmt.Errorf("%w: %s is unset in registry %s",
				ErrUnregistered, k.Name, registryAddress)
		}
		p.contracts[k.Hash] = Contract{Key: k, Address: addr}
	}
	return p, results[len(keys):], nil
}

// Get returns a resolved contract.
func (p Protocol) Get(k Key) (Contract, error) {
	c, ok := p.contracts[k.Hash]
	if !ok {
		return Contract{}, fmt.Errorf("%w: %s (resolved: %s)", ErrNotResolved, k.Name, p.resolvedNames())
	}
	if k.ABI == "" {
		return c, fmt.Errorf("%w: %s", ErrNoABI, k.Name)
	}
	return c, nil
}

// Address returns a resolved contract's address without requiring an ABI, for
// keys that are only ever compared or logged.
func (p Protocol) AddressOf(k Key) (common.Address, error) {
	c, ok := p.contracts[k.Hash]
	if !ok {
		return common.Address{}, fmt.Errorf("%w: %s (resolved: %s)", ErrNotResolved, k.Name, p.resolvedNames())
	}
	return c.Address, nil
}

// MustGet is Get for call sites that listed the key in their own Resolve call,
// where a miss is a programming error rather than a chain state.
//
// It still returns an error-free Contract with a zero address on a miss rather
// than panicking, because panicking inside a WASM workflow loses the log line
// that would explain it.
func (p Protocol) MustGet(k Key) Contract {
	c, err := p.Get(k)
	if err != nil {
		return Contract{Key: k}
	}
	return c
}

// Typed accessors, mirroring `Auth`'s helpers. Each returns an error when the
// key was not part of this tick's Resolve, which is a caller bug rather than a
// chain condition — hence the name in the message.
func (p Protocol) Controller() (Contract, error)      { return p.Get(Controller) }
func (p Protocol) ExitQueue() (Contract, error)       { return p.Get(ExitQueue) }
func (p Protocol) AMM() (Contract, error)             { return p.Get(AMM) }
func (p Protocol) StrategyManager() (Contract, error) { return p.Get(StrategyManager) }
func (p Protocol) Oracle() (Contract, error)          { return p.Get(Oracle) }
func (p Protocol) QueueKeeperExecutor() (Contract, error) {
	return p.Get(QueueKeeperExecutor)
}
func (p Protocol) StrategyKeeperExecutor() (Contract, error) {
	return p.Get(StrategyKeeperExecutor)
}

// Contracts returns every resolved contract, sorted by name, for logging the
// address book a tick actually used.
func (p Protocol) Contracts() []Contract {
	out := make([]Contract, 0, len(p.contracts))
	for _, c := range p.contracts {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.Name < out[j].Key.Name })
	return out
}

// LogAttrs renders the address book as structured log pairs.
func (p Protocol) LogAttrs() []any {
	attrs := []any{"registry", p.Address.Hex()}
	for _, c := range p.Contracts() {
		attrs = append(attrs, strings.ToLower(c.Key.Name), c.Address.Hex())
	}
	return attrs
}

func (p Protocol) resolvedNames() string {
	cs := p.Contracts()
	if len(cs) == 0 {
		return "none"
	}
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Key.Name
	}
	return strings.Join(out, ", ")
}

func names(keys []Key) string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.Name
	}
	return strings.Join(out, ", ")
}
