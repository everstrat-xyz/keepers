// Package envelope encodes and decodes the EverStrat CRE report envelope so W1
// and W2 produce byte-identical reports for `CREReceiverBase.onReport`.
//
// The on-chain type is:
//
//	struct Envelope {
//	    uint64 chainSelector;
//	    uint64 sequence;
//	    uint64 observedAt;
//	    uint8  action;
//	    bytes  params;
//	}
//
// and the receiver does `abi.decode(report, (Envelope))`, so the report body is
// exactly `abi.encode(envelope)` — a single dynamic tuple, i.e. a leading 0x20
// offset word followed by the tuple body.
//
// # Hard constraint
//
// `params` carries claims and hints only. It must never carry an authoritative
// ETH amount, NAV, or price: `CREStrategyExecutor` ignores params entirely and
// recomputes every quantity, and `CREQueueExecutor` re-derives the affordable
// range before calling the Controller. Encode helpers in pkg/queue and
// pkg/strategy are the supported way to build params; they cannot express an
// amount. See docs/envelope.md.
package envelope

import (
	"errors"
	"fmt"
	"math"
	"time"

	gethabi "github.com/ethereum/go-ethereum/accounts/abi"
)

// Envelope mirrors ICREReceiverBase.Envelope.
type Envelope struct {
	// ChainSelector is the CCIP chain selector of the destination chain. The
	// receiver rejects anything but its own immutable CHAIN_SELECTOR.
	ChainSelector uint64
	// Sequence must strictly increase per receiver: onReport rejects
	// `sequence <= lastSequence`.
	Sequence uint64
	// ObservedAt is the unix second the workflow observed the state the report
	// claims. It must not be in the future and must be within MAX_REPORT_AGE
	// of the delivering block's timestamp.
	ObservedAt uint64
	// Action is the receiver-specific action enum value.
	Action uint8
	// Params are ABI-encoded hints. Never amounts.
	Params []byte
}

// Errors returned by Decode and Validate. They mirror the receiver's reverts so
// a workflow can refuse to emit a report the contract would reject anyway.
var (
	ErrWrongChain        = errors.New("envelope: chainSelector does not match receiver CHAIN_SELECTOR")
	ErrReplayedSequence  = errors.New("envelope: sequence must be strictly greater than lastSequence")
	ErrObservedInFuture  = errors.New("envelope: observedAt is in the future")
	ErrStaleReport       = errors.New("envelope: observedAt is older than MAX_REPORT_AGE")
	ErrZeroChainSelector = errors.New("envelope: chainSelector must be non-zero")
	ErrZeroSequence      = errors.New("envelope: sequence must be non-zero")
	ErrZeroObservedAt    = errors.New("envelope: observedAt must be non-zero")
)

// abiArgs is `abi.encode(Envelope)` as a go-ethereum argument list: one dynamic
// tuple. Field order and types must stay identical to the Solidity struct.
var abiArgs = gethabi.Arguments{{Type: mustTupleType()}}

func mustTupleType() gethabi.Type {
	t, err := gethabi.NewType("tuple", "", []gethabi.ArgumentMarshaling{
		{Name: "chainSelector", Type: "uint64"},
		{Name: "sequence", Type: "uint64"},
		{Name: "observedAt", Type: "uint64"},
		{Name: "action", Type: "uint8"},
		{Name: "params", Type: "bytes"},
	})
	if err != nil {
		panic(fmt.Sprintf("envelope: building Envelope tuple type: %v", err))
	}
	return t
}

// abiEnvelope is the struct go-ethereum packs/unpacks the tuple through. Field
// names must match the capitalised ABI component names.
type abiEnvelope struct {
	ChainSelector uint64
	Sequence      uint64
	ObservedAt    uint64
	Action        uint8
	Params        []byte
}

// Encode returns the `report` bytes to hand to `writeReport`.
//
// Encode does not enforce staleness — ObservedAt is compared against the
// delivering block, which the workflow cannot know. Call Validate against the
// receiver's live state before emitting.
func (e Envelope) Encode() ([]byte, error) {
	if e.ChainSelector == 0 {
		return nil, ErrZeroChainSelector
	}
	if e.Sequence == 0 {
		return nil, ErrZeroSequence
	}
	if e.ObservedAt == 0 {
		return nil, ErrZeroObservedAt
	}
	b, err := abiArgs.Pack(abiEnvelope(e))
	if err != nil {
		return nil, fmt.Errorf("envelope: encoding: %w", err)
	}
	return b, nil
}

// Decode parses report bytes produced by Encode (or by the Solidity test
// helpers) back into an Envelope.
func Decode(report []byte) (Envelope, error) {
	vals, err := abiArgs.Unpack(report)
	if err != nil {
		return Envelope{}, fmt.Errorf("envelope: decoding: %w", err)
	}
	// Arguments.Copy writes a single non-tuple argument into field 0 of the
	// destination struct, hence the wrapper.
	var out struct{ Envelope abiEnvelope }
	if err := abiArgs.Copy(&out, vals); err != nil {
		return Envelope{}, fmt.Errorf("envelope: decoding: %w", err)
	}
	return Envelope{
		ChainSelector: out.Envelope.ChainSelector,
		Sequence:      out.Envelope.Sequence,
		ObservedAt:    out.Envelope.ObservedAt,
		Action:        out.Envelope.Action,
		Params:        out.Envelope.Params,
	}, nil
}

// ReceiverState is the live receiver state a report is validated against. Read
// it from the receiver (`CHAIN_SELECTOR`, `lastSequence`, `MAX_REPORT_AGE`)
// rather than assuming config values match the deployment.
type ReceiverState struct {
	ChainSelector uint64
	LastSequence  uint64
	MaxReportAge  uint64 // seconds
}

// Validate applies every guard `CREReceiverBase.onReport` applies, against a
// caller-supplied "now" standing in for the delivering block's timestamp.
//
// Because delivery lands some blocks after the workflow runs, `now` should be
// the earliest plausible delivery time and the caller should leave headroom in
// MAX_REPORT_AGE — see Deadline and RemainingBudget.
func (e Envelope) Validate(state ReceiverState, now time.Time) error {
	if e.ChainSelector != state.ChainSelector {
		return fmt.Errorf("%w: report %d, receiver %d", ErrWrongChain, e.ChainSelector, state.ChainSelector)
	}
	if e.Sequence <= state.LastSequence {
		return fmt.Errorf("%w: report %d, lastSequence %d", ErrReplayedSequence, e.Sequence, state.LastSequence)
	}

	nowUnix := now.Unix()
	if nowUnix < 0 {
		return fmt.Errorf("envelope: invalid now %s", now)
	}
	ts := uint64(nowUnix)

	if e.ObservedAt > ts {
		return fmt.Errorf("%w: observedAt %d, now %d", ErrObservedInFuture, e.ObservedAt, ts)
	}
	if age := ts - e.ObservedAt; age > state.MaxReportAge {
		return fmt.Errorf("%w: age %ds, MAX_REPORT_AGE %ds", ErrStaleReport, age, state.MaxReportAge)
	}
	return nil
}

// NextSequence returns the lowest sequence the receiver will accept.
//
// Sequence is per-receiver and monotonic in `lastSequence`, not a counter the
// workflow owns: read `lastSequence()` each run rather than persisting a local
// count, so a workflow restart or a manual break-glass report cannot wedge the
// keeper behind the receiver.
func NextSequence(lastSequence uint64) (uint64, error) {
	if lastSequence == math.MaxUint64 {
		return 0, errors.New("envelope: lastSequence is uint64 max; receiver can accept no further reports")
	}
	return lastSequence + 1, nil
}

// Deadline returns the latest block timestamp at which a report observed at
// observedAt can still be delivered: `observedAt + MAX_REPORT_AGE`.
//
// A workflow should refuse to emit a report whose deadline it cannot plausibly
// beat. Such a report cannot corrupt state — the receiver reverts — but it
// spends credits and hides the real upkeep signal behind a failed delivery.
func Deadline(observedAt time.Time, maxReportAge uint64) time.Time {
	return observedAt.Add(time.Duration(maxReportAge) * time.Second)
}

// RemainingBudget returns how much of MAX_REPORT_AGE is left at now. It goes
// negative once the report can no longer land.
func RemainingBudget(observedAt time.Time, maxReportAge uint64, now time.Time) time.Duration {
	return Deadline(observedAt, maxReportAge).Sub(now)
}

// DeliveryMargin is the headroom a report needs beyond consensus and
// transmission before emitting is worthwhile.
//
// Validation alone is not enough: a report validated at build time can still
// be stale by the time the DON agrees on it and the transaction lands, because
// MAX_REPORT_AGE is consumed by delivery latency, not by build time. The
// observedAt -> deadline budget is hours (MAX_REPORT_AGE is at least a
// constructor-enforced non-zero value, and chains.MaxReportAgeCeiling caps
// config mirrors at 24h), so a two-minute margin is a small fraction of it
// while comfortably covering a DON round trip.
const DeliveryMargin = 2 * time.Minute

// CanPlausiblyDeliver reports whether a report observed at observedAt still
// has more than DeliveryMargin of MAX_REPORT_AGE left at now.
//
// The receiver's staleness revert is guaranteed either way; this exists so a
// report that would only just make it is skipped at build time instead of
// burning credits on a delivery that races the deadline.
func CanPlausiblyDeliver(observedAt time.Time, maxReportAge uint64, now time.Time) bool {
	return RemainingBudget(observedAt, maxReportAge, now) > DeliveryMargin
}
