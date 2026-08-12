// Package abi vendors the EverStrat contract ABIs the keeper workflows need and
// exposes them as lazily-parsed go-ethereum ABI values.
//
// The JSON files next to this one are copied verbatim from `forge build` output
// in everstrat-xyz/contracts — see SOURCE.md for the pinned commit and the
// refresh procedure. Do not hand-edit them.
//
// Two groups live here:
//
//   - Receivers (ICREReceiverBase, ICREQueueExecutor, ICREStrategyExecutor) —
//     the contracts W1/W2 deliver reports to, plus their gas-bounded
//     `*UpkeepStatus` cross-check views.
//   - Reads (IRegistry, IController, IExitQueue, IAMM, IStrategyManager) —
//     the off-chain view surface workflows scan before proposing an action.
package abi

import (
	"bytes"
	"embed"
	"fmt"
	"sync"

	gethabi "github.com/ethereum/go-ethereum/accounts/abi"
)

//go:embed *.json
var files embed.FS

// Name identifies a vendored ABI. Values match the JSON file base names.
type Name string

const (
	ICREReceiverBase     Name = "ICREReceiverBase"
	ICREQueueExecutor    Name = "ICREQueueExecutor"
	ICREStrategyExecutor Name = "ICREStrategyExecutor"

	IRegistry        Name = "IRegistry"
	IController      Name = "IController"
	IExitQueue       Name = "IExitQueue"
	IAMM             Name = "IAMM"
	IStrategyManager Name = "IStrategyManager"

	// Pausable is the OpenZeppelin `paused()` fragment. It is hand-written
	// rather than vendored: the EverStrat interfaces inherit Pausable without
	// re-declaring it, so it appears in no forge artifact — yet both
	// `*UpkeepStatus` views gate on it, so the keepers must read it too.
	Pausable Name = "Pausable"

	// Multicall3 is the canonical aggregator, hand-written from the published
	// interface rather than vendored from EverStrat's build.
	//
	// It exists here because CRE caps a workflow at 15 contract reads per
	// execution (ChainRead.CallLimit). Batching sub-calls through aggregate3 is
	// what makes a queue scan deeper than the on-chain view's window possible
	// at all — see pkg/evmread.
	Multicall3 Name = "Multicall3"
)

// All lists every vendored ABI. Used by tests to assert the embed set stays
// in sync with the constants above.
var All = []Name{
	ICREReceiverBase,
	ICREQueueExecutor,
	ICREStrategyExecutor,
	IRegistry,
	IController,
	IExitQueue,
	IAMM,
	IStrategyManager,
	Pausable,
	Multicall3,
}

type parsed struct {
	abi gethabi.ABI
	err error
}

var (
	mu    sync.Mutex
	cache = map[Name]parsed{}
)

// JSON returns the raw vendored ABI bytes.
func JSON(name Name) ([]byte, error) {
	b, err := files.ReadFile(string(name) + ".json")
	if err != nil {
		return nil, fmt.Errorf("abi: no vendored ABI named %q: %w", name, err)
	}
	return b, nil
}

// Get returns the parsed ABI. Results are cached; the returned value must be
// treated as read-only.
func Get(name Name) (gethabi.ABI, error) {
	mu.Lock()
	defer mu.Unlock()

	if p, ok := cache[name]; ok {
		return p.abi, p.err
	}

	var p parsed
	raw, err := JSON(name)
	if err != nil {
		p.err = err
	} else if p.abi, p.err = gethabi.JSON(bytes.NewReader(raw)); p.err != nil {
		p.err = fmt.Errorf("abi: parsing %s: %w", name, p.err)
	}

	cache[name] = p
	return p.abi, p.err
}

// MustGet is Get for package-level initialisation, panicking on a vendoring
// mistake that no runtime input can cause.
func MustGet(name Name) gethabi.ABI {
	a, err := Get(name)
	if err != nil {
		panic(err)
	}
	return a
}

// Method looks up a single method, giving a clearer error than a nil-map read
// when an ABI is refreshed and a signature moves.
func Method(name Name, method string) (gethabi.Method, error) {
	a, err := Get(name)
	if err != nil {
		return gethabi.Method{}, err
	}
	m, ok := a.Methods[method]
	if !ok {
		return gethabi.Method{}, fmt.Errorf("abi: %s has no method %q", name, method)
	}
	return m, nil
}
