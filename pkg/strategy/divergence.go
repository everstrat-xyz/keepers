package strategy

import (
	"fmt"
	"math/big"
)

// UpkeepStatus is the result of the on-chain `strategyUpkeepStatus()` view.
type UpkeepStatus struct {
	Action Action
	Amount *big.Int
}

// DivergenceClass labels how a workflow decision relates to the on-chain view.
//
// W2's bar is stricter than W1's: because `_processReport` re-derives every
// quantity with the same bounded helpers the view uses, any action disagreement
// means a report that reverts. There is no equivalent of W1's "found work the
// view could not see" — see Decide for why.
type DivergenceClass string

const (
	// DivergenceMatch — the workflow and the view agree on the action.
	DivergenceMatch DivergenceClass = "match"

	// DivergenceAmountOnly — same action, different amount. Harmless by
	// construction: the report carries no amount and the receiver recomputes
	// it, so only the action has to agree. Usually means the read snapshot and
	// the view were taken a block apart.
	DivergenceAmountOnly DivergenceClass = "amount-only"

	// DivergenceTruncatedScan — the read budget could not cover the full
	// bounded scan, so NeedsETH understates and the disagreement is explained
	// by the workflow's own incomplete read rather than by a logic error.
	DivergenceTruncatedScan DivergenceClass = "truncated-scan"

	// DivergenceBug — an unexplained action disagreement. This is what must
	// stay at zero through the shadow window.
	DivergenceBug DivergenceClass = "bug"
)

// Divergence is the classified comparison, ready to log.
type Divergence struct {
	Class       DivergenceClass
	Decision    Decision
	OnChain     UpkeepStatus
	Explanation string
}

// Unexplained reports whether this should count against the shadow window.
func (d Divergence) Unexplained() bool { return d.Class == DivergenceBug }

// LogAttrs renders the divergence as flat key/value pairs for structured
// logging, matching the shape pkg/queue emits so one dashboard query covers
// both keepers.
func (d Divergence) LogAttrs() []any {
	return []any{
		"divergence", string(d.Class),
		"workflowAction", d.Decision.Action.String(),
		"workflowAmount", orZero(d.Decision.Amount).String(),
		"workflowReason", d.Decision.Reason,
		"onchainAction", d.OnChain.Action.String(),
		"onchainAmount", orZero(d.OnChain.Amount).String(),
		"explanation", d.Explanation,
	}
}

// Classify compares a workflow decision against the on-chain view.
func Classify(decision Decision, onChain UpkeepStatus, s State) Divergence {
	d := Divergence{Class: DivergenceBug, Decision: decision, OnChain: onChain}

	if decision.Action == onChain.Action {
		if orZero(decision.Amount).Cmp(orZero(onChain.Amount)) == 0 {
			d.Class = DivergenceMatch
			d.Explanation = "same action and amount"
			return d
		}
		d.Class = DivergenceAmountOnly
		d.Explanation = fmt.Sprintf(
			"same action; amounts differ (%s vs %s wei), which the report does not carry and the receiver recomputes",
			orZero(decision.Amount), orZero(onChain.Amount))
		return d
	}

	if s.ScanTruncated {
		d.Class = DivergenceTruncatedScan
		d.Explanation = fmt.Sprintf(
			"read budget truncated the redemption scan, so pending needs are understated; workflow proposes %s, view recommends %s",
			decision.Action, onChain.Action)
		return d
	}

	d.Explanation = fmt.Sprintf(
		"workflow proposes %s but the on-chain view recommends %s — the receiver re-derives with the same bounded helpers, so this report would revert",
		decision.Action, onChain.Action)
	return d
}
