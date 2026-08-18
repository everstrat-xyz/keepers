package strategy_test

import (
	"testing"
	"time"

	"github.com/everstrat-xyz/keepers/pkg/envelope"
	"github.com/everstrat-xyz/keepers/pkg/strategy"
)

// These tests reproduce, over real encoded report bytes, the gate sequence
// strategy-keeper's main runs between deciding and writing:
//
//	NextSequence -> Report.Build -> envelope.Decode -> Validate -> CanPlausiblyDeliver
//
// The workflow main is //go:build wasip1 and excluded from host tests, so this
// is the closest executable check that the W2 pipeline emits reports the
// receiver accepts and skips the ones it would only narrowly accept. W2's
// params are always empty by design — the receiver recomputes every amount —
// which is the property under test here.

const (
	sel = uint64(16015286601757825753) // Sepolia CCIP selector
	age = uint64(3600)
)

// preflight mirrors strategy-keeper/main.go's buildReport + the two gates.
// lastSequenceAtBuild is the value read this tick; lastSequenceAtDelivery is
// what the receiver holds when the report lands.
func preflight(
	t *testing.T,
	a strategy.Action,
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
	encoded, err := strategy.Report{
		ChainSelector: sel,
		Sequence:      sequence,
		ObservedAt:    uint64(now.Unix()),
	}.Build(a)
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

func TestPreflightEmitsValidReportsForEveryAction(t *testing.T) {
	observed := time.Unix(1700000000, 0)

	for a := strategy.ActionRebalance; a <= strategy.ActionProvideExitLiquidity; a++ {
		t.Run(a.String(), func(t *testing.T) {
			e, emit, err := preflight(t, a, 41, 41, observed, observed.Add(5*time.Minute))
			if err != nil || !emit {
				t.Fatalf("preflight() = emit %v, err %v; want true, nil", emit, err)
			}
			if e.Action != uint8(a) {
				t.Errorf("decoded action = %d, want %d (%s)", e.Action, a, a)
			}
			if e.Sequence != 42 {
				t.Errorf("decoded sequence = %d, want 42 (receiver lastSequence 41 + 1)", e.Sequence)
			}
			// W2 reports carry no amounts, on the wire: the entire params field
			// is empty. If this ever fails, an authoritative amount has reached
			// the report builder.
			if len(e.Params) != 0 {
				t.Errorf("params = 0x%x, want empty — a W2 report must never carry params", e.Params)
			}
		})
	}
}

func TestPreflightSkipsReportThatRacesTheDeadline(t *testing.T) {
	observed := time.Unix(1700000000, 0)

	// Validate accepts at deadline minus 30s; the delivery margin does not.
	e, emit, err := preflight(t, strategy.ActionRebalance, 41, 41, observed, observed.Add(time.Hour-30*time.Second))
	if err != nil {
		t.Fatalf("Validate rejected a report the receiver would accept: %v", err)
	}
	if emit {
		t.Fatal("preflight emitted a report with under DeliveryMargin of budget left")
	}
	if e.Action != uint8(strategy.ActionRebalance) {
		t.Errorf("skipped report lost its action: %d", e.Action)
	}
}

func TestPreflightRejectsReplayedSequence(t *testing.T) {
	observed := time.Unix(1700000000, 0)

	// Sequence built from 41, but the receiver has since moved to 42 — a
	// break-glass report landed first. Our 42 is now a replay.
	_, _, err := preflight(t, strategy.ActionSync, 41, 42, observed, observed.Add(time.Minute))
	if err == nil {
		t.Fatal("preflight accepted a sequence the receiver already consumed")
	}
}
