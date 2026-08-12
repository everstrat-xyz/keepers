// Package queue builds W1 (queue-keeper) reports for `CREQueueExecutor`.
//
// Every action's params are ABI-encoded exactly as the receiver decodes them:
//
//	PriceBatch     abi.encode(uint256 batchId)
//	ProcessRequests abi.encode(uint256 batchId, uint256 startIndex, uint256 endIndex)  // endIndex exclusive
//	AdvanceCursor  abi.encode(uint256 batchId)
//
// # No amounts
//
// The params surface here is closed: the only values that can be expressed are
// a batch id and an index range, all uint64-typed on the Go side. There is no
// path to put an ETH amount, NAV, or price into a W1 report, and DecodeParams
// rejects any params blob longer than the action's exact encoding — which is
// what an appended amount word would look like on the wire.
package queue

import (
	"errors"
	"fmt"
	"math/big"

	gethabi "github.com/ethereum/go-ethereum/accounts/abi"

	"github.com/everstrat-xyz/keepers/pkg/envelope"
)

// Action mirrors ICREQueueExecutor.QueueAction. Values are the Solidity enum
// ordinals and must not be reordered.
type Action uint8

const (
	// ActionNone is the enum zero value. queueUpkeepStatus returns it when
	// there is nothing to do; it is never a valid report action —
	// _processReport reverts KeeperExecutorUnknownAction.
	ActionNone Action = 0
	// ActionPriceBatch prices the current batch once it is at least
	// minBatchAge old.
	ActionPriceBatch Action = 1
	// ActionProcessRequests settles a prefix of a priced batch's affordable
	// requests.
	ActionProcessRequests Action = 2
	// ActionAdvanceCursor skips fully-processed or expired batches.
	ActionAdvanceCursor Action = 3
)

func (a Action) String() string {
	switch a {
	case ActionNone:
		return "None"
	case ActionPriceBatch:
		return "PriceBatch"
	case ActionProcessRequests:
		return "ProcessRequests"
	case ActionAdvanceCursor:
		return "AdvanceCursor"
	default:
		return fmt.Sprintf("Action(%d)", uint8(a))
	}
}

// Valid reports whether a is an action the receiver will process.
func (a Action) Valid() bool {
	return a == ActionPriceBatch || a == ActionProcessRequests || a == ActionAdvanceCursor
}

var (
	ErrUnknownAction = errors.New("queue: unknown or non-actionable QueueAction")
	// ErrStartIndexNotZero guards the receiver's `startIndex != 0` revert:
	// CREQueueExecutor only accepts a claimed range that is a prefix of the
	// affordable set.
	ErrStartIndexNotZero = errors.New("queue: startIndex must be 0 (receiver only accepts an affordable-set prefix)")
	ErrEmptyRange        = errors.New("queue: endIndex must be greater than startIndex")
	ErrParamsLength      = errors.New("queue: params length does not match the action's encoding")
)

var (
	uint256Type = mustType("uint256")

	priceBatchArgs = gethabi.Arguments{
		{Name: "batchId", Type: uint256Type},
	}
	processRequestsArgs = gethabi.Arguments{
		{Name: "batchId", Type: uint256Type},
		{Name: "startIndex", Type: uint256Type},
		{Name: "endIndex", Type: uint256Type},
	}
	advanceCursorArgs = gethabi.Arguments{
		{Name: "batchId", Type: uint256Type},
	}
)

func mustType(t string) gethabi.Type {
	typ, err := gethabi.NewType(t, "", nil)
	if err != nil {
		panic(fmt.Sprintf("queue: building %s type: %v", t, err))
	}
	return typ
}

// Params is the decoded, typed view of a W1 report's params.
//
// StartIndex/EndIndex are only meaningful for ActionProcessRequests. Note the
// deliberate absence of any value field.
type Params struct {
	Action     Action
	BatchID    uint64
	StartIndex uint64
	EndIndex   uint64 // exclusive
}

// EncodePriceBatchParams encodes `abi.encode(batchId)`.
func EncodePriceBatchParams(batchID uint64) ([]byte, error) {
	b, err := priceBatchArgs.Pack(new(big.Int).SetUint64(batchID))
	if err != nil {
		return nil, fmt.Errorf("queue: encoding PriceBatch params: %w", err)
	}
	return b, nil
}

// EncodeProcessRequestsParams encodes `abi.encode(batchId, startIndex, endIndex)`
// with endIndex exclusive, matching Controller.processRequests.
//
// startIndex is fixed at 0 rather than taken as an argument: the receiver
// re-derives the affordable set from live state and reverts unless the claimed
// range starts at 0. A workflow may claim a shorter prefix (endIndex below the
// affordable count) but never an offset one.
func EncodeProcessRequestsParams(batchID, endIndex uint64) ([]byte, error) {
	if endIndex == 0 {
		return nil, ErrEmptyRange
	}
	b, err := processRequestsArgs.Pack(
		new(big.Int).SetUint64(batchID),
		new(big.Int).SetUint64(0),
		new(big.Int).SetUint64(endIndex),
	)
	if err != nil {
		return nil, fmt.Errorf("queue: encoding ProcessRequests params: %w", err)
	}
	return b, nil
}

// EncodeAdvanceCursorParams encodes `abi.encode(batchId)`, where batchId is the
// cursor position the workflow expects the receiver to reach. The receiver
// reverts if it cannot advance at least that far.
func EncodeAdvanceCursorParams(batchID uint64) ([]byte, error) {
	b, err := advanceCursorArgs.Pack(new(big.Int).SetUint64(batchID))
	if err != nil {
		return nil, fmt.Errorf("queue: encoding AdvanceCursor params: %w", err)
	}
	return b, nil
}

// argsFor returns the exact argument list for an action.
func argsFor(a Action) (gethabi.Arguments, error) {
	switch a {
	case ActionPriceBatch:
		return priceBatchArgs, nil
	case ActionProcessRequests:
		return processRequestsArgs, nil
	case ActionAdvanceCursor:
		return advanceCursorArgs, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownAction, a)
	}
}

// DecodeParams parses params for an action and enforces the exact wire length.
//
// The length check is the "no smuggled amounts" guard: all three params layouts
// are static, so a correct blob is exactly 32 bytes per field. An appended
// amount word — the mistake this package exists to make impossible — changes
// the length and is rejected here rather than being silently ignored by
// Solidity's abi.decode.
func DecodeParams(a Action, params []byte) (Params, error) {
	args, err := argsFor(a)
	if err != nil {
		return Params{}, err
	}
	if want := len(args) * 32; len(params) != want {
		return Params{}, fmt.Errorf("%w: %s wants %d bytes, got %d", ErrParamsLength, a, want, len(params))
	}

	vals, err := args.Unpack(params)
	if err != nil {
		return Params{}, fmt.Errorf("queue: decoding %s params: %w", a, err)
	}

	out := Params{Action: a}
	nums := make([]uint64, len(vals))
	for i, v := range vals {
		n, ok := v.(*big.Int)
		if !ok {
			return Params{}, fmt.Errorf("queue: decoding %s params: field %d is %T, want *big.Int", a, i, v)
		}
		if !n.IsUint64() {
			return Params{}, fmt.Errorf("queue: decoding %s params: field %d overflows uint64 (%s)", a, i, n)
		}
		nums[i] = n.Uint64()
	}

	out.BatchID = nums[0]
	if a == ActionProcessRequests {
		out.StartIndex, out.EndIndex = nums[1], nums[2]
		if out.StartIndex != 0 {
			return Params{}, fmt.Errorf("%w: got %d", ErrStartIndexNotZero, out.StartIndex)
		}
		if out.EndIndex <= out.StartIndex {
			return Params{}, ErrEmptyRange
		}
	}
	return out, nil
}

// Report bundles envelope headers with a W1 action.
type Report struct {
	// ChainSelector is the destination chain's CCIP selector.
	ChainSelector uint64
	// Sequence must be envelope.NextSequence(receiver.lastSequence()).
	Sequence uint64
	// ObservedAt is the unix second of the state observation this report claims.
	ObservedAt uint64
}

// PriceBatch builds the full report bytes for a PriceBatch action.
func (r Report) PriceBatch(batchID uint64) ([]byte, error) {
	params, err := EncodePriceBatchParams(batchID)
	if err != nil {
		return nil, err
	}
	return r.encode(ActionPriceBatch, params)
}

// ProcessRequests builds the full report bytes for a ProcessRequests action
// claiming the first endIndex affordable requests of batchID.
func (r Report) ProcessRequests(batchID, endIndex uint64) ([]byte, error) {
	params, err := EncodeProcessRequestsParams(batchID, endIndex)
	if err != nil {
		return nil, err
	}
	return r.encode(ActionProcessRequests, params)
}

// AdvanceCursor builds the full report bytes for an AdvanceCursor action.
func (r Report) AdvanceCursor(batchID uint64) ([]byte, error) {
	params, err := EncodeAdvanceCursorParams(batchID)
	if err != nil {
		return nil, err
	}
	return r.encode(ActionAdvanceCursor, params)
}

func (r Report) encode(a Action, params []byte) ([]byte, error) {
	// Re-decode before shipping: cheap, and it fails loudly if a future edit
	// makes a builder and its wire layout disagree.
	if _, err := DecodeParams(a, params); err != nil {
		return nil, err
	}
	return envelope.Envelope{
		ChainSelector: r.ChainSelector,
		Sequence:      r.Sequence,
		ObservedAt:    r.ObservedAt,
		Action:        uint8(a),
		Params:        params,
	}.Encode()
}
