package queue_test

import (
	"strings"
	"testing"

	"github.com/everstrat-xyz/keepers/pkg/queue"
)

func TestClassify(t *testing.T) {
	// A plain state with one processable batch at the cursor — the on-chain
	// view can see everything here, so any disagreement is a bug.
	plain := stateWith(batch(1, req(1, 1), req(2, 1)))

	tests := []struct {
		name     string
		decision queue.Decision
		onChain  queue.UpkeepStatus
		state    queue.State
		want     queue.DivergenceClass
	}{
		{
			name:     "both idle",
			decision: queue.Decision{Action: queue.ActionNone},
			onChain:  queue.UpkeepStatus{Action: queue.ActionNone},
			state:    plain,
			want:     queue.DivergenceMatch,
		},
		{
			name:     "same batch and prefix",
			decision: queue.Decision{Action: queue.ActionProcessRequests, BatchID: 1, EndIndex: 2},
			onChain:  queue.UpkeepStatus{Action: queue.ActionProcessRequests, BatchID: 1, Count: 2},
			state:    plain,
			want:     queue.DivergenceMatch,
		},
		{
			name:     "same PriceBatch",
			decision: queue.Decision{Action: queue.ActionPriceBatch, BatchID: 1},
			onChain:  queue.UpkeepStatus{Action: queue.ActionPriceBatch, BatchID: 1},
			state:    plain,
			want:     queue.DivergenceMatch,
		},
		{
			// The receiver accepts any prefix of the affordable set, so
			// claiming fewer is safe.
			name:     "shorter prefix claimed",
			decision: queue.Decision{Action: queue.ActionProcessRequests, BatchID: 1, EndIndex: 1},
			onChain:  queue.UpkeepStatus{Action: queue.ActionProcessRequests, BatchID: 1, Count: 2},
			state:    plain,
			want:     queue.DivergenceIntendedImprovement,
		},
		{
			// Over-claiming reverts KeeperExecutorNoUpkeepNeeded on-chain.
			name:     "longer prefix claimed",
			decision: queue.Decision{Action: queue.ActionProcessRequests, BatchID: 1, EndIndex: 5},
			onChain:  queue.UpkeepStatus{Action: queue.ActionProcessRequests, BatchID: 1, Count: 2},
			state:    plain,
			want:     queue.DivergenceBug,
		},
		{
			name:     "workflow idle while the view has work",
			decision: queue.Decision{Action: queue.ActionNone},
			onChain:  queue.UpkeepStatus{Action: queue.ActionProcessRequests, BatchID: 1, Count: 2},
			state:    plain,
			want:     queue.DivergenceBug,
		},
		{
			name:     "different batches for the same action",
			decision: queue.Decision{Action: queue.ActionProcessRequests, BatchID: 2, EndIndex: 1},
			onChain:  queue.UpkeepStatus{Action: queue.ActionProcessRequests, BatchID: 1, Count: 2},
			state:    plain,
			want:     queue.DivergenceBug,
		},
		{
			name:     "different actions entirely",
			decision: queue.Decision{Action: queue.ActionPriceBatch, BatchID: 1},
			onChain:  queue.UpkeepStatus{Action: queue.ActionAdvanceCursor, BatchID: 3},
			state:    plain,
			want:     queue.DivergenceBug,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := queue.Classify(tt.decision, tt.onChain, tt.state)
			if got.Class != tt.want {
				t.Errorf("Class = %s, want %s (explanation: %s)", got.Class, tt.want, got.Explanation)
			}
			if got.Explanation == "" {
				t.Error("Explanation is empty; shadow-mode triage depends on it")
			}
		})
	}
}

// TestClassifyBeyondWindowIsAnImprovement is the divergence that must NOT count
// against the shadow window: the off-chain full scan finds a processable batch
// past where queueUpkeepStatus stops looking, so the view reports None while
// the workflow proposes real work.
func TestClassifyBeyondWindowIsAnImprovement(t *testing.T) {
	var batches []queue.Batch
	for id := uint64(1); id <= 60; id++ {
		b := batch(id, req(1, 1))
		b.UnprocessedCount = 0
		batches = append(batches, b)
	}
	batches = append(batches, batch(61, req(9, 1)))

	s := stateWith(batches...)
	s.CurrentBatchID = 62

	decision, err := queue.Decide(s)
	if err != nil {
		t.Fatal(err)
	}

	// The gas-bounded view walks its cursor to 1+25=26, scans [26, 51), finds
	// only processed batches there, and reports None.
	got := queue.Classify(decision, queue.UpkeepStatus{Action: queue.ActionNone}, s)

	if got.Class != queue.DivergenceIntendedImprovement {
		t.Errorf("Class = %s, want %s (explanation: %s)", got.Class, queue.DivergenceIntendedImprovement, got.Explanation)
	}
	if got.Unexplained() {
		t.Error("Unexplained() = true; a full-scan win must not count against the 7-day shadow window")
	}
	if !strings.Contains(got.Explanation, "beyond the on-chain scan window") {
		t.Errorf("Explanation = %q, want it to name the scan window", got.Explanation)
	}
}

func TestUnexplained(t *testing.T) {
	for _, tt := range []struct {
		class queue.DivergenceClass
		want  bool
	}{
		{queue.DivergenceMatch, false},
		{queue.DivergenceIntendedImprovement, false},
		{queue.DivergenceBug, true},
	} {
		d := queue.Divergence{Class: tt.class}
		if got := d.Unexplained(); got != tt.want {
			t.Errorf("%s.Unexplained() = %v, want %v", tt.class, got, tt.want)
		}
	}
}

// TestLogAttrsArePairs guards the structured-log contract the shadow-mode
// dashboard query depends on.
func TestLogAttrsArePairs(t *testing.T) {
	d := queue.Classify(
		queue.Decision{Action: queue.ActionProcessRequests, BatchID: 1, EndIndex: 2, Reason: "because"},
		queue.UpkeepStatus{Action: queue.ActionProcessRequests, BatchID: 1, Count: 2},
		stateWith(batch(1, req(1, 1), req(2, 1))),
	)

	attrs := d.LogAttrs()
	if len(attrs)%2 != 0 {
		t.Fatalf("LogAttrs() returned %d values, want an even number of key/value pairs", len(attrs))
	}

	keys := map[string]bool{}
	for i := 0; i < len(attrs); i += 2 {
		k, ok := attrs[i].(string)
		if !ok {
			t.Fatalf("LogAttrs()[%d] = %v, want a string key", i, attrs[i])
		}
		if keys[k] {
			t.Errorf("duplicate log key %q", k)
		}
		keys[k] = true
	}
	for _, want := range []string{"divergence", "workflowAction", "onchainAction", "explanation"} {
		if !keys[want] {
			t.Errorf("LogAttrs() is missing key %q", want)
		}
	}
}
