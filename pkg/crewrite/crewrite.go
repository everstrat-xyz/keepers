// Package crewrite performs the DON-signed `writeReport` delivery shared by
// W1 and W2.
//
// # Shadow mode is the default everywhere
//
// Nothing here decides whether to write — callers gate on their own
// `shadowMode` config first. This package only knows how to write once that
// decision is made, so "did we mean to go live?" stays a single, greppable
// check in each workflow rather than a flag buried in a helper.
//
// # Reading the reply
//
// A successful transaction is not a successful upkeep. The forwarder can land
// the transaction while the receiver reverts — `KeeperExecutorNoUpkeepNeeded`
// is the common and *expected* case, since the receiver re-validates every
// claim against live state. Callers get both statuses so they can alert on a
// revert *rate* rather than on any single revert.
package crewrite

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"

	"github.com/smartcontractkit/cre-sdk-go/capabilities/blockchain/evm"
	"github.com/smartcontractkit/cre-sdk-go/cre"
)

// DefaultGasLimit is the gas ceiling for a receiver call.
//
// `ProcessRequests` is the expensive action: it loops over the claimed prefix
// and makes an external call per request, so the limit has to cover
// maxUsersPerUpkeep redemptions plus the cursor advance.
const DefaultGasLimit = uint64(2_000_000)

// Result describes a delivery attempt.
type Result struct {
	// TxStatus is whether the transaction landed at all.
	TxStatus evm.TxStatus
	// ReceiverReverted is true when the transaction landed but the receiver
	// rejected the report. Expected during normal operation.
	ReceiverReverted bool
	TxHash           common.Hash
	ErrorMessage     string
}

// Succeeded reports whether the report was both delivered and accepted.
func (r Result) Succeeded() bool {
	return r.TxStatus == evm.TxStatus_TX_STATUS_SUCCESS && !r.ReceiverReverted
}

// ErrEmptyReport guards against writing a zero-length payload, which would
// revert on `abi.decode` inside the receiver and waste a delivery.
var ErrEmptyReport = errors.New("crewrite: report payload is empty")

// Write signs the payload into a DON report and delivers it to the receiver.
//
// The payload must be the ABI-encoded Envelope from pkg/envelope; this adds the
// CRE report wrapper (workflow identity metadata and F+1 signatures) that the
// KeystoneForwarder verifies before calling `onReport`.
func Write(
	runtime cre.Runtime,
	client *evm.Client,
	receiver common.Address,
	payload []byte,
	gasLimit uint64,
) (Result, error) {
	if len(payload) == 0 {
		return Result{}, ErrEmptyReport
	}
	if gasLimit == 0 {
		gasLimit = DefaultGasLimit
	}

	report, err := runtime.GenerateReport(&cre.ReportRequest{
		EncodedPayload: payload,
		EncoderName:    "evm",
		SigningAlgo:    "ecdsa",
		HashingAlgo:    "keccak256",
	}).Await()
	if err != nil {
		return Result{}, fmt.Errorf("crewrite: generating DON report: %w", err)
	}

	reply, err := client.WriteReport(runtime, &evm.WriteCreReportRequest{
		Receiver:  receiver.Bytes(),
		Report:    report,
		GasConfig: &evm.GasConfig{GasLimit: gasLimit},
	}).Await()
	if err != nil {
		return Result{}, fmt.Errorf("crewrite: writing report to %s: %w", receiver, err)
	}

	out := Result{TxStatus: reply.TxStatus}
	if reply.TxHash != nil {
		out.TxHash = common.BytesToHash(reply.TxHash)
	}
	if reply.ErrorMessage != nil {
		out.ErrorMessage = *reply.ErrorMessage
	}
	if s := reply.ReceiverContractExecutionStatus; s != nil {
		out.ReceiverReverted = *s == evm.ReceiverContractExecutionStatus_RECEIVER_CONTRACT_EXECUTION_STATUS_REVERTED
	}
	return out, nil
}
