package queue

import "fmt"

// UpkeepStatus is the result of the on-chain `queueUpkeepStatus()` cross-check
// view.
type UpkeepStatus struct {
	Action  Action
	BatchID uint64
	Count   uint64
}

// DivergenceClass labels how a workflow decision relates to the on-chain view.
//
// Shadow mode's exit criterion is "zero unexplained divergences over 7 days"
// (issue #5), which only means something if "explained" is defined in code
// rather than judged per incident. These are those definitions.
type DivergenceClass string

const (
	// DivergenceMatch — the workflow and the on-chain view agree.
	DivergenceMatch DivergenceClass = "match"

	// DivergenceIntendedImprovement — the workflow found work the gas-bounded
	// view structurally could not see, or claimed a valid shorter prefix.
	// Expected, and the reason W1 exists.
	DivergenceIntendedImprovement DivergenceClass = "intended-improvement"

	// DivergenceBug — anything else. The workflow disagrees with the view in a
	// way the scan window does not explain, so either the off-chain model or
	// the read layer is wrong. This is what must stay at zero.
	DivergenceBug DivergenceClass = "bug"
)

// Divergence is the classified comparison, ready to log.
type Divergence struct {
	Class    DivergenceClass
	Decision Decision
	OnChain  UpkeepStatus
	// Explanation says why the class was chosen, in the terms an operator
	// triaging a shadow-mode log needs.
	Explanation string
}

// LogAttrs renders the divergence as flat key/value pairs for structured
// logging, so a daily "unexplained divergence count" is a log query rather than
// a parsing exercise.
func (d Divergence) LogAttrs() []any {
	return []any{
		"divergence", string(d.Class),
		"workflowAction", d.Decision.Action.String(),
		"workflowBatchId", d.Decision.BatchID,
		"workflowEndIndex", d.Decision.EndIndex,
		"workflowReason", d.Decision.Reason,
		"onchainAction", d.OnChain.Action.String(),
		"onchainBatchId", d.OnChain.BatchID,
		"onchainCount", d.OnChain.Count,
		"explanation", d.Explanation,
	}
}

// Unexplained reports whether this divergence should count against the shadow
// window.
func (d Divergence) Unexplained() bool { return d.Class == DivergenceBug }

// Classify compares a workflow decision against the on-chain view.
//
// The window argument is the state the decision was made from; it is needed to
// tell "the view could not see this batch" from "the view disagrees about this
// batch".
func Classify(decision Decision, onChain UpkeepStatus, s State) Divergence {
	d := Divergence{Class: DivergenceBug, Decision: decision, OnChain: onChain}

	boundedCursor := s.PeekAdvancedCursor(MaxBatchScan)
	windowEnd := s.OnChainScanWindowEnd()

	switch {
	case decision.Action == ActionNone && onChain.Action == ActionNone:
		d.Class = DivergenceMatch
		d.Explanation = "both agree there is no upkeep"

	case decision.Action == ActionProcessRequests && onChain.Action == ActionProcessRequests &&
		decision.BatchID == onChain.BatchID:
		switch {
		case decision.EndIndex == onChain.Count:
			d.Class = DivergenceMatch
			d.Explanation = "same batch and same affordable prefix"
		case decision.EndIndex < onChain.Count:
			// The receiver accepts any prefix, so a shorter claim is safe.
			d.Class = DivergenceIntendedImprovement
			d.Explanation = fmt.Sprintf("claiming a shorter prefix (%d of %d affordable) — accepted by the receiver",
				decision.EndIndex, onChain.Count)
		default:
			d.Explanation = fmt.Sprintf("claiming %d requests but the on-chain view finds only %d affordable — the receiver would revert KeeperExecutorNoUpkeepNeeded",
				decision.EndIndex, onChain.Count)
		}

	case decision.Action == ActionProcessRequests && decision.BatchID >= windowEnd:
		// The genuine full-scan win: this batch is past where the view stops.
		d.Class = DivergenceIntendedImprovement
		d.Explanation = fmt.Sprintf("batch %d is beyond the on-chain scan window (cursor %d + %d) — found by the off-chain full scan",
			decision.BatchID, boundedCursor, MaxBatchScan)

	case decision.Action == onChain.Action && decision.BatchID == onChain.BatchID:
		d.Class = DivergenceMatch
		d.Explanation = "same action and batch"

	case decision.Action == ActionNone && onChain.Action != ActionNone:
		d.Explanation = fmt.Sprintf("on-chain view recommends %s on batch %d but the workflow proposes nothing — upkeep would stall",
			onChain.Action, onChain.BatchID)

	case decision.Action == ActionAdvanceCursor && decision.BatchID > windowEnd:
		// Decide caps AdvanceCursor at the bounded cursor, so this means the
		// cap was bypassed and the receiver could not reach the claim.
		d.Explanation = fmt.Sprintf("cursor claim %d exceeds what the receiver can reach in one report (bounded cursor %d)",
			decision.BatchID, boundedCursor)

	default:
		d.Explanation = fmt.Sprintf("workflow proposes %s on batch %d, on-chain view recommends %s on batch %d, and the scan window does not explain the difference",
			decision.Action, decision.BatchID, onChain.Action, onChain.BatchID)
	}

	return d
}
