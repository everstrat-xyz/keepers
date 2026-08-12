// Package evmread wraps the CRE EVM capability with ABI packing and unpacking
// against the vendored contract ABIs.
//
// # Why promises are returned rather than values
//
// `runtime.CallCapability` dispatches to the host immediately and only blocks
// in `Await`. Firing every call in a round and awaiting afterwards therefore
// costs one round trip instead of N — which is the difference between a
// full-queue scan being practical and being a timeout. Call returns a promise
// so callers can keep that shape; see AwaitAll.
//
// # Block selection
//
// Reads default to the finalized block. Every node in the DON has to observe
// the same state for consensus to succeed, and "latest" differs between nodes
// by construction. The cost is that observations lag finality (~13 minutes on
// Sepolia), which is why MAX_REPORT_AGE has to be comfortably larger than that
// — see docs/envelope.md.
package evmread

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	valuespb "github.com/smartcontractkit/chainlink-protos/cre/go/values/pb"
	"github.com/smartcontractkit/cre-sdk-go/capabilities/blockchain/evm"
	"github.com/smartcontractkit/cre-sdk-go/capabilities/blockchain/evm/bindings"
	"github.com/smartcontractkit/cre-sdk-go/cre"

	everabi "github.com/everstrat-xyz/keepers/contracts/evm/src/abi"
)

// BlockTag selects which block reads observe.
type BlockTag string

const (
	// BlockFinalized is the default and the only one safe for DON consensus.
	BlockFinalized BlockTag = "finalized"
	// BlockLatest trades consensus safety for freshness. Only for local
	// simulation, where there is a single node.
	BlockLatest BlockTag = "latest"
)

// Caller issues contract reads on one chain.
type Caller struct {
	client  *evm.Client
	runtime cre.Runtime
	block   *valuespb.BigInt
}

// New builds a Caller for a chain selector.
func New(runtime cre.Runtime, chainSelector uint64, tag BlockTag) *Caller {
	block := bindings.FinalizedBlockNumber
	if tag == BlockLatest {
		block = bindings.LatestBlockNumber
	}
	return &Caller{
		client:  &evm.Client{ChainSelector: chainSelector},
		runtime: runtime,
		block:   block,
	}
}

// Call packs a method call, dispatches it, and returns a promise for the
// decoded return values.
//
// The call is already in flight when this returns; nothing further happens
// until Await.
func (c *Caller) Call(to common.Address, name everabi.Name, method string, args ...any) cre.Promise[[]any] {
	m, err := everabi.Method(name, method)
	if err != nil {
		return cre.PromiseFromResult[[]any](nil, err)
	}

	parsed, err := everabi.Get(name)
	if err != nil {
		return cre.PromiseFromResult[[]any](nil, err)
	}

	data, err := parsed.Pack(method, args...)
	if err != nil {
		return cre.PromiseFromResult[[]any](nil, fmt.Errorf("evmread: packing %s.%s: %w", name, method, err))
	}

	reply := c.client.CallContract(c.runtime, &evm.CallContractRequest{
		Call:        &evm.CallMsg{To: to.Bytes(), Data: data},
		BlockNumber: c.block,
	})

	return cre.Then(reply, func(r *evm.CallContractReply) ([]any, error) {
		out, err := m.Outputs.Unpack(r.Data)
		if err != nil {
			return nil, fmt.Errorf("evmread: unpacking %s.%s from %s: %w", name, method, to, err)
		}
		return out, nil
	})
}

// CallOne is Call for the common single-return-value case.
func (c *Caller) CallOne(to common.Address, name everabi.Name, method string, args ...any) cre.Promise[any] {
	return cre.Then(c.Call(to, name, method, args...), func(vals []any) (any, error) {
		if len(vals) != 1 {
			return nil, fmt.Errorf("evmread: %s.%s returned %d values, want 1", name, method, len(vals))
		}
		return vals[0], nil
	})
}

// BalanceAt reads an address's ETH balance at the configured block.
func (c *Caller) BalanceAt(addr common.Address) cre.Promise[*big.Int] {
	reply := c.client.BalanceAt(c.runtime, &evm.BalanceAtRequest{
		Account:     addr.Bytes(),
		BlockNumber: c.block,
	})
	return cre.Then(reply, func(r *evm.BalanceAtReply) (*big.Int, error) {
		if r.Balance == nil {
			return nil, fmt.Errorf("evmread: nil balance for %s", addr)
		}
		return valuespb.NewIntFromBigInt(r.Balance), nil
	})
}

// BlockTimestamp reads the observed block's unix timestamp.
//
// This — not `runtime.Now()` — is the clock the keepers must use, for two
// reasons:
//
//  1. Age comparisons against chain state (a batch's `createdAt` versus
//     `minBatchAge`) are only meaningful against the same clock the contract
//     used to record them.
//  2. `CREReceiverBase` rejects `observedAt > block.timestamp` with no
//     tolerance. `runtime.Now()` is the DON's wall clock, which can sit ahead
//     of the chain; taking `observedAt` from the observed block guarantees it
//     is behind the delivering block instead of racing it.
func (c *Caller) BlockTimestamp() cre.Promise[uint64] {
	reply := c.client.HeaderByNumber(c.runtime, &evm.HeaderByNumberRequest{
		BlockNumber: c.block,
	})
	return cre.Then(reply, func(r *evm.HeaderByNumberReply) (uint64, error) {
		if r.Header == nil {
			return 0, fmt.Errorf("evmread: header reply has no header")
		}
		if r.Header.Timestamp == 0 {
			return 0, fmt.Errorf("evmread: header has a zero timestamp")
		}
		return r.Header.Timestamp, nil
	})
}

// Client exposes the underlying capability client for writes.
func (c *Caller) Client() *evm.Client { return c.client }

// Runtime exposes the runtime the Caller was built with.
func (c *Caller) Runtime() cre.Runtime { return c.runtime }

// ---------- decoding helpers ----------
//
// go-ethereum's Unpack returns `any`, so every read site would otherwise repeat
// the same type assertion and produce a nil-pointer panic on a shape change.
// These convert once, with an error that names the field.

// Uint64 converts an ABI uint256 return value, rejecting anything that does not
// fit. Batch ids, indices and timestamps are all uint64-shaped in practice, and
// a value that overflows means the read is wrong rather than the chain being
// exotic.
func Uint64(v any, field string) (uint64, error) {
	n, ok := v.(*big.Int)
	if !ok {
		return 0, fmt.Errorf("evmread: %s is %T, want *big.Int", field, v)
	}
	if !n.IsUint64() {
		return 0, fmt.Errorf("evmread: %s = %s overflows uint64", field, n)
	}
	return n.Uint64(), nil
}

// BigInt converts an ABI uint256 return value that must keep full width — ETH
// amounts and prices, where truncating to uint64 would corrupt the value.
func BigInt(v any, field string) (*big.Int, error) {
	n, ok := v.(*big.Int)
	if !ok {
		return nil, fmt.Errorf("evmread: %s is %T, want *big.Int", field, v)
	}
	return n, nil
}

// Bool converts an ABI bool return value.
func Bool(v any, field string) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("evmread: %s is %T, want bool", field, v)
	}
	return b, nil
}

// Address converts an ABI address return value.
func Address(v any, field string) (common.Address, error) {
	a, ok := v.(common.Address)
	if !ok {
		return common.Address{}, fmt.Errorf("evmread: %s is %T, want common.Address", field, v)
	}
	return a, nil
}

// Addresses converts an ABI address[] return value.
func Addresses(v any, field string) ([]common.Address, error) {
	a, ok := v.([]common.Address)
	if !ok {
		return nil, fmt.Errorf("evmread: %s is %T, want []common.Address", field, v)
	}
	return a, nil
}

// AwaitAll resolves a slice of already-dispatched promises, returning the first
// error with its index.
//
// The whole point is the call order: build the entire slice first (every call
// is then in flight at once), and only then AwaitAll. Awaiting inside the
// build loop serialises the round trips and turns a full-queue scan into a
// timeout.
func AwaitAll[T any](promises []cre.Promise[T]) ([]T, error) {
	out := make([]T, len(promises))
	for i, p := range promises {
		v, err := p.Await()
		if err != nil {
			return nil, fmt.Errorf("evmread: call %d of %d: %w", i+1, len(promises), err)
		}
		out[i] = v
	}
	return out, nil
}

// Paused reads OpenZeppelin's `paused()`.
//
// Both `*UpkeepStatus` views return None when the Controller, ExitQueue or AMM
// is paused, and every action would revert anyway, so the keepers have to check
// the same flags to agree with the views.
func (c *Caller) Paused(addr common.Address) cre.Promise[bool] {
	return cre.Then(c.CallOne(addr, everabi.Pausable, "paused"), func(v any) (bool, error) {
		return Bool(v, "paused")
	})
}
