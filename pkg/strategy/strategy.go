// Package strategy builds W2 (strategy-keeper) reports for
// `CREStrategyExecutor`.
//
// # No amounts — structurally
//
// `CREStrategyExecutor._processReport` takes `bytes memory /* params */` and
// never reads it: every ETH quantity (shortfall, excess, top-up, fee) is
// recomputed from live Controller / StrategyManager / ExitQueue / AMM state at
// execution time. A W2 report therefore carries an action and nothing else.
//
// This package encodes that as an API with no params argument at all, and
// ValidateParams rejects any non-empty params blob. If a future workflow needs
// to pass a hint, it has to change this package — and the contract — on
// purpose, which is exactly the review gate the hard constraint asks for.
package strategy

import (
	"errors"
	"fmt"

	"github.com/everstrat-xyz/keepers/pkg/envelope"
)

// Action mirrors ICREStrategyExecutor.StrategyAction. Values are the Solidity
// enum ordinals and must not be reordered.
type Action uint8

const (
	// ActionNone is the enum zero value returned by strategyUpkeepStatus when
	// there is nothing to do. Never a valid report action.
	ActionNone Action = 0
	// ActionRebalance triggers Controller.checkAndRebalanceStrategies.
	ActionRebalance Action = 1
	// ActionWithdrawShortfall pulls ETH from strategies to cover pending
	// redemptions.
	ActionWithdrawShortfall Action = 2
	// ActionDepositExcess deploys idle Controller ETH above the reserve.
	ActionDepositExcess Action = 3
	// ActionHarvestPerformanceFees harvests accrued performance fees.
	ActionHarvestPerformanceFees Action = 4
	// ActionSync refreshes strategy accounting once syncInterval has elapsed.
	ActionSync Action = 5
	// ActionProvideExitLiquidity tops the AMM exit side up toward its target.
	ActionProvideExitLiquidity Action = 6
)

// Priority is the order CREStrategyExecutor.strategyUpkeepStatus evaluates
// actions in. A workflow that proposes its own action rather than mirroring the
// view should follow the same order, so on-chain and off-chain agree on which
// single action a given state calls for.
var Priority = []Action{
	ActionRebalance,
	ActionWithdrawShortfall,
	ActionProvideExitLiquidity,
	ActionDepositExcess,
	ActionHarvestPerformanceFees,
	ActionSync,
}

func (a Action) String() string {
	switch a {
	case ActionNone:
		return "None"
	case ActionRebalance:
		return "Rebalance"
	case ActionWithdrawShortfall:
		return "WithdrawShortfall"
	case ActionDepositExcess:
		return "DepositExcess"
	case ActionHarvestPerformanceFees:
		return "HarvestPerformanceFees"
	case ActionSync:
		return "Sync"
	case ActionProvideExitLiquidity:
		return "ProvideExitLiquidity"
	default:
		return fmt.Sprintf("Action(%d)", uint8(a))
	}
}

// Valid reports whether a is an action the receiver will process.
func (a Action) Valid() bool {
	return a >= ActionRebalance && a <= ActionProvideExitLiquidity
}

var (
	ErrUnknownAction = errors.New("strategy: unknown or non-actionable StrategyAction")
	// ErrParamsNotEmpty is the "no amounts" guard. W2 reports carry no params;
	// a non-empty blob means someone tried to ship an amount the contract will
	// ignore, which is a correctness trap rather than a harmless extra.
	ErrParamsNotEmpty = errors.New("strategy: params must be empty (amounts are recomputed on-chain)")
)

// ValidateParams enforces the empty-params rule.
func ValidateParams(params []byte) error {
	if len(params) != 0 {
		return fmt.Errorf("%w: got %d bytes", ErrParamsNotEmpty, len(params))
	}
	return nil
}

// Report bundles envelope headers with a W2 action.
type Report struct {
	// ChainSelector is the destination chain's CCIP selector.
	ChainSelector uint64
	// Sequence must be envelope.NextSequence(receiver.lastSequence()).
	Sequence uint64
	// ObservedAt is the unix second of the state observation this report claims.
	ObservedAt uint64
}

// Build returns the full report bytes for an action. There is deliberately no
// params argument.
func (r Report) Build(a Action) ([]byte, error) {
	if !a.Valid() {
		return nil, fmt.Errorf("%w: %s", ErrUnknownAction, a)
	}
	return envelope.Envelope{
		ChainSelector: r.ChainSelector,
		Sequence:      r.Sequence,
		ObservedAt:    r.ObservedAt,
		Action:        uint8(a),
		Params:        nil,
	}.Encode()
}
