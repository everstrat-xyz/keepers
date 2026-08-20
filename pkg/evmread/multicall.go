package evmread

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"github.com/smartcontractkit/cre-sdk-go/cre"

	everabi "github.com/everstrat-xyz/keepers/contracts/evm/src/abi"
)

// CRE per-execution read limits, from `cre workflow limits export`.
//
// These are not advisory. Exceeding CallLimit aborts the execution with
// `Public:User:LimitExceeded`, so the read plan has to be budgeted up front
// rather than discovered at runtime.
const (
	// MaxChainReads is ChainRead.CallLimit: contract reads per execution.
	MaxChainReads = 15
	// MaxReadPayloadBytes is ChainRead.PayloadSizeLimit.
	MaxReadPayloadBytes = 5 * 1024
)

// Multicall3Address is the canonical CREATE2 deployment, identical on every
// chain EverStrat targets (verified live on Ethereum mainnet and Sepolia).
var Multicall3Address = common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11")

// SubCall is one read inside a Multicall3 batch.
type SubCall struct {
	To     common.Address
	ABI    everabi.Name
	Method string
	Args   []any
}

// SubResult is one decoded sub-call result.
type SubResult struct {
	// Success is false when the sub-call reverted and the batch allowed it.
	Success bool
	// Values are the decoded return values. Empty when Success is false.
	Values []any
}

// Address, Uint64, Uint8, BigInt and Bool decode a sub-call that returns exactly one
// value — the overwhelmingly common case in every read plan.
//
// The arity check is the point: a sub-call whose ABI changed shape returns a
// different number of values, and without this the type assertion behind it
// would panic on an index that is no longer there. Each returns an error naming
// the field so a failed read says which one.

func (r SubResult) Address(field string) (common.Address, error) {
	if err := r.expectOne(field); err != nil {
		return common.Address{}, err
	}
	return Address(r.Values[0], field)
}

func (r SubResult) Uint64(field string) (uint64, error) {
	if err := r.expectOne(field); err != nil {
		return 0, err
	}
	return Uint64(r.Values[0], field)
}

// Uint8 decodes a Solidity uint8 (e.g. StrategyManager.depositWeight), which
// go-ethereum unpacks as a native uint8 rather than a big.Int.
func (r SubResult) Uint8(field string) (uint8, error) {
	if err := r.expectOne(field); err != nil {
		return 0, err
	}
	v, ok := r.Values[0].(uint8)
	if !ok {
		return 0, fmt.Errorf("evmread: %s is %T, want uint8", field, r.Values[0])
	}
	return v, nil
}

func (r SubResult) BigInt(field string) (*big.Int, error) {
	if err := r.expectOne(field); err != nil {
		return nil, err
	}
	return BigInt(r.Values[0], field)
}

func (r SubResult) Bool(field string) (bool, error) {
	if err := r.expectOne(field); err != nil {
		return false, err
	}
	return Bool(r.Values[0], field)
}

func (r SubResult) expectOne(field string) error {
	if len(r.Values) != 1 {
		return fmt.Errorf("evmread: %s returned %d values, want 1", field, len(r.Values))
	}
	return nil
}

// aggregate3Call mirrors Multicall3.Call3 for ABI packing.
type aggregate3Call struct {
	Target       common.Address `abi:"target"`
	AllowFailure bool           `abi:"allowFailure"`
	CallData     []byte         `abi:"callData"`
}

// resultOverheadBytes is the ABI framing cost of one Result in the returned
// array: the element offset word, the success word, the returnData offset and
// length words, plus slack for padding.
const resultOverheadBytes = 5 * 32

// EstimateResultBytes returns the response size one sub-call contributes, used
// to chunk batches under MaxReadPayloadBytes.
//
// Static return types are sized exactly from the ABI. Dynamic ones (notably
// `address[]`) cannot be, so callers pass an expected element count.
func EstimateResultBytes(returnBytes int) int {
	return resultOverheadBytes + ((returnBytes + 31) / 32 * 32)
}

// Aggregate batches sub-calls into a single Multicall3 `aggregate3` read.
//
// This is one chain read regardless of how many sub-calls it carries, which is
// the entire reason it exists: the alternative is one read per sub-call against
// a budget of 15.
//
// allowFailure applies to every sub-call. Pass false when a revert means the
// read plan is wrong (the usual case) and true when reverts are expected and
// the caller handles them per result.
//
// Callers must keep the response under MaxReadPayloadBytes — see ChunkSubCalls.
func (c *Caller) Aggregate(calls []SubCall, allowFailure bool) cre.Promise[[]SubResult] {
	if len(calls) == 0 {
		return cre.PromiseFromResult([]SubResult{}, nil)
	}

	packed := make([]aggregate3Call, len(calls))
	for i, sc := range calls {
		parsed, err := everabi.Get(sc.ABI)
		if err != nil {
			return cre.PromiseFromResult[[]SubResult](nil, err)
		}
		data, err := parsed.Pack(sc.Method, sc.Args...)
		if err != nil {
			return cre.PromiseFromResult[[]SubResult](nil,
				fmt.Errorf("evmread: packing %s.%s for multicall: %w", sc.ABI, sc.Method, err))
		}
		packed[i] = aggregate3Call{Target: sc.To, AllowFailure: allowFailure, CallData: data}
	}

	reply := c.Call(Multicall3Address, everabi.Multicall3, "aggregate3", packed)

	return cre.Then(reply, func(vals []any) ([]SubResult, error) {
		if len(vals) != 1 {
			return nil, fmt.Errorf("evmread: aggregate3 returned %d values, want 1", len(vals))
		}

		// go-ethereum unpacks tuple[] into an anonymous struct slice it
		// generates itself, so this assertion has to name that exact shape —
		// a declared mirror type would not match.
		raw, ok := vals[0].([]struct {
			Success    bool   `json:"success"`
			ReturnData []byte `json:"returnData"`
		})
		if !ok {
			return nil, fmt.Errorf("evmread: aggregate3 result is %T, want a Result tuple slice", vals[0])
		}
		if len(raw) != len(calls) {
			return nil, fmt.Errorf("evmread: aggregate3 returned %d results for %d calls", len(raw), len(calls))
		}

		out := make([]SubResult, len(raw))
		for i, r := range raw {
			out[i] = SubResult{Success: r.Success}
			if !r.Success {
				if !allowFailure {
					return nil, fmt.Errorf("evmread: %s.%s on %s reverted inside multicall",
						calls[i].ABI, calls[i].Method, calls[i].To)
				}
				continue
			}
			m, err := everabi.Method(calls[i].ABI, calls[i].Method)
			if err != nil {
				return nil, err
			}
			decoded, err := m.Outputs.Unpack(r.ReturnData)
			if err != nil {
				return nil, fmt.Errorf("evmread: unpacking %s.%s from multicall: %w",
					calls[i].ABI, calls[i].Method, err)
			}
			out[i].Values = decoded
		}
		return out, nil
	})
}

// ChunkSubCalls splits sub-calls into batches whose responses stay under
// MaxReadPayloadBytes, given a per-result size estimate.
//
// perResultBytes should come from EstimateResultBytes for the widest return
// type in the set; mixing wide and narrow returns in one batch is fine as long
// as the estimate covers the widest.
func ChunkSubCalls(calls []SubCall, perResultBytes int) [][]SubCall {
	if len(calls) == 0 {
		return nil
	}
	perBatch := MaxReadPayloadBytes / perResultBytes
	if perBatch < 1 {
		perBatch = 1
	}

	var out [][]SubCall
	for start := 0; start < len(calls); start += perBatch {
		end := start + perBatch
		if end > len(calls) {
			end = len(calls)
		}
		out = append(out, calls[start:end])
	}
	return out
}

// Budget tracks the per-execution chain-read allowance so a read plan can be
// trimmed before it is issued rather than aborting the execution partway
// through.
type Budget struct {
	remaining int
}

// NewBudget returns a budget with `reserve` reads held back for work the caller
// must be able to perform later — typically the cross-check view and the
// receiver reads a report cannot be built without.
func NewBudget(reserve int) *Budget {
	r := MaxChainReads - reserve
	if r < 0 {
		r = 0
	}
	return &Budget{remaining: r}
}

// Remaining reports the reads still available.
func (b *Budget) Remaining() int { return b.remaining }

// Take consumes n reads, reporting whether the budget allowed it.
func (b *Budget) Take(n int) bool {
	if n > b.remaining {
		return false
	}
	b.remaining -= n
	return true
}

// TakeUpTo consumes as many of n reads as remain, returning how many were
// granted. Used where a partial scan is better than none.
func (b *Budget) TakeUpTo(n int) int {
	if n > b.remaining {
		n = b.remaining
	}
	b.remaining -= n
	return n
}
