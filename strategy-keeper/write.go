//go:build wasip1

package main

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"

	"github.com/smartcontractkit/cre-sdk-go/cre"

	"github.com/everstrat-xyz/keepers/pkg/crewrite"
	"github.com/everstrat-xyz/keepers/pkg/evmread"
)

// writeReport delivers a report to the strategy executor and returns the tx hash.
//
// A receiver revert is reported as a successful *tick* with a logged warning
// rather than a workflow error: `KeeperExecutorNoUpkeepNeeded` means the
// on-chain state moved between observation and delivery, which is the system
// working as designed. Alerting belongs on the revert rate over time (W4,
// issue #7), not on a single occurrence.
func writeReport(
	runtime cre.Runtime,
	caller *evmread.Caller,
	receiver common.Address,
	payload []byte,
) (string, error) {
	res, err := crewrite.Write(runtime, caller.Client(), receiver, payload, 0)
	if err != nil {
		return "", err
	}

	logger := runtime.Logger()
	switch {
	case res.Succeeded():
		return res.TxHash.Hex(), nil
	case res.ReceiverReverted:
		logger.Warn("receiver rejected the report — state moved between observation and delivery",
			"txHash", res.TxHash.Hex(),
			"error", res.ErrorMessage,
		)
		return res.TxHash.Hex(), nil
	default:
		return "", fmt.Errorf("report delivery failed: status=%s error=%q", res.TxStatus, res.ErrorMessage)
	}
}
