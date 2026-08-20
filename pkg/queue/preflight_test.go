package queue_test

import (
	"testing"
	"time"

	"github.com/everstrat-xyz/keepers/pkg/envelope"
	"github.com/everstrat-xyz/keepers/pkg/queue"
)

// These tests reproduce, over real encoded report bytes, the gate sequence
// queue-keeper's main runs between deciding and writing:
//
//	NextSequence -> Report.* -> envelope.Decode -> Validate -> CanPlausiblyDeliver
//
// The workflow main itself is //go:build wasip1 and excluded from host tests,
// so this is the closest executable check that the pipeline, as composed,
// emits reports the receiver accepts — and skips the ones it would only
// narrowly accept. The end-to-end check against a live receiver remains the
// fork harness (docs/LOCAL_FORK.md).

const (
	sel = uint64(16015286601757825753) // Sepolia CCIP selector
	age = uint64(3600)
)

// preflight mirrors queue-keeper/main.go's buildReport + the two gates.
// lastSequenceAtBuild is the value read this tick; lastSequenceAtDelivery is
// what the receiver holds when the report lands — a break-glass report can
// move it in between, which is exactly the race the sequence guard exists for.
// A nil error with emit=false is the "skip with a message" path; a non-nil
// error is the "return err" path.
func preflight(
	t *testing.T,
	d queue.Decision,
	lastSequenceAtBuild uint64,
	lastSequenceAtDelivery uint64,
	now time.Time,
	deliveryAt time.Time,
) (envelope.Envelope, bool, error) {
	t.Helper()

	sequence, err := envelope.NextSequence(lastSequenceAtBuild)
	if err != nil {
		return envelope.Envelope{}, false, err
	}
	r := queue.Report{ChainSelector: sel, Sequence: sequence, ObservedAt: uint64(now.Unix())}

	var encoded []byte
	switch d.Action {
	case queue.ActionPriceBatch:
		encoded, err = r.PriceBatch(d.BatchID)
	case queue.ActionProcessRequests:
		encoded, err = r.ProcessRequests(d.BatchID, d.EndIndex)
	case queue.ActionAdvanceCursor:
		encoded, err = r.AdvanceCursor(d.BatchID)
	default:
		t.Fatalf("unsupported action %s", d.Action)
	}
	if err != nil {
		return envelope.Envelope{}, false, err
	}

	decoded, err := envelope.Decode(encoded)
	if err != nil {
		return envelope.Envelope{}, false, err
	}

	state := envelope.ReceiverState{ChainSelector: sel, LastSequence: lastSequenceAtDelivery, MaxReportAge: age}
	if err := decoded.Validate(state, deliveryAt); err != nil {
		return decoded, false, err
	}
	if !envelope.CanPlausiblyDeliver(now, age, deliveryAt) {
		return decoded, false, nil
	}
	return decoded, true, nil
}

func TestPreflightEmitsValidReports(t *testing.T) {
	observed := time.Unix(1700000000, 0)

	decisions := []queue.Decision{
		{Action: queue.ActionPriceBatch, BatchID: 7},
		{Action: queue.ActionProcessRequests, BatchID: 7, EndIndex: 12},
		{Action: queue.ActionAdvanceCursor, BatchID: 3},
	}
	for _, d := range decisions {
		t.Run(d.Action.String(), func(t *testing.T) {
			e, emit, err := preflight(t, d, 41, 41, observed, observed.Add(5*time.Minute))
			if err != nil || !emit {
				t.Fatalf("preflight() = emit %v, err %v; want true, nil", emit, err)
			}
			if e.Action != uint8(d.Action) {
				t.Errorf("decoded action = %d, want %d (%s)", e.Action, d.Action, d.Action)
			}
			if e.Sequence != 42 {
				t.Errorf("decoded sequence = %d, want 42 (receiver lastSequence 41 + 1)", e.Sequence)
			}

			// The wire form must survive the receiver's own params decode, with
			// its exact-length rule against smuggled amounts.
			p, err := queue.DecodeParams(d.Action, e.Params)
			if err != nil {
				t.Fatalf("DecodeParams() error = %v", err)
			}
			if p.BatchID != d.BatchID {
				t.Errorf("params batchId = %d, want %d", p.BatchID, d.BatchID)
			}
			if d.Action == queue.ActionProcessRequests && p.EndIndex != d.EndIndex {
				t.Errorf("params endIndex = %d, want %d", p.EndIndex, d.EndIndex)
			}
		})
	}
}

func TestPreflightSkipsReportThatRacesTheDeadline(t *testing.T) {
	observed := time.Unix(1700000000, 0)
	d := queue.Decision{Action: queue.ActionPriceBatch, BatchID: 7}

	// Validate still accepts here — deadline minus 30s is inside MAX_REPORT_AGE —
	// but the delivery margin does not: consensus plus transmission cannot be
	// counted on to beat 30 seconds.
	e, emit, err := preflight(t, d, 41, 41, observed, observed.Add(time.Hour-30*time.Second))
	if err != nil {
		t.Fatalf("Validate rejected a report the receiver would accept: %v", err)
	}
	if emit {
		t.Fatal("preflight emitted a report with under DeliveryMargin of budget left")
	}
	// The skip path still returns the decoded envelope so the tick logs what
	// it decided not to send.
	if e.Action != uint8(queue.ActionPriceBatch) {
		t.Errorf("skipped report lost its action: %d", e.Action)
	}
}

func TestPreflightRejectsReplayedSequence(t *testing.T) {
	observed := time.Unix(1700000000, 0)
	d := queue.Decision{Action: queue.ActionAdvanceCursor, BatchID: 3}

	// Sequence built from 41 (read this tick), but a break-glass report from
	// the multisig KEEPER_ROLE path landed first and moved the receiver to
	// 42. Our 42 is now a replay; Validate must refuse.
	_, _, err := preflight(t, d, 41, 42, observed, observed.Add(time.Minute))
	if err == nil {
		t.Fatal("preflight accepted a sequence the receiver already consumed")
	}
}
