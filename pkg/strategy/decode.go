package strategy

import (
	"fmt"

	"github.com/everstrat-xyz/keepers/pkg/evmread"
)

// DecodeUpkeepStatus converts `CREStrategyExecutor.strategyUpkeepStatus()`:
//
//	(StrategyAction action, uint256 amount)
//
// The action is a Solidity enum, which the ABI reports as uint8.
func DecodeUpkeepStatus(vals []any) (UpkeepStatus, error) {
	if len(vals) != 2 {
		return UpkeepStatus{}, fmt.Errorf("strategy: strategyUpkeepStatus returned %d values, want 2", len(vals))
	}
	action, ok := vals[0].(uint8)
	if !ok {
		return UpkeepStatus{}, fmt.Errorf("strategy: strategyUpkeepStatus action is %T, want uint8", vals[0])
	}
	amount, err := evmread.BigInt(vals[1], "strategyUpkeepStatus.amount")
	if err != nil {
		return UpkeepStatus{}, err
	}
	return UpkeepStatus{Action: Action(action), Amount: amount}, nil
}
