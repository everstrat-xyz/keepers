package queue

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"

	"github.com/everstrat-xyz/keepers/pkg/evmread"
)

// This file holds the conversions from raw ABI return values into decision
// inputs. It lives here rather than in the workflow package so it is reachable
// from host tests: the workflow mains are //go:build wasip1, and these
// conversions are exactly where a contract-side signature change turns into a
// silent misread.

// DecodeBatchInfo converts `ExitQueue.batchInfo(uint256)`:
//
//	(bool canBeProcessed, uint256 finalEvePrice, uint256 totalTokensToBurn,
//	 uint256 createdAt, uint256 pricedAt)
//
// UnprocessedCount and Requests are filled in by later reads.
func DecodeBatchInfo(id uint64, vals []any) (Batch, error) {
	if len(vals) != 5 {
		return Batch{}, fmt.Errorf("queue: batchInfo(%d) returned %d values, want 5", id, len(vals))
	}

	canBeProcessed, err := evmread.Bool(vals[0], "batchInfo.canBeProcessed")
	if err != nil {
		return Batch{}, err
	}
	finalEvePrice, err := evmread.BigInt(vals[1], "batchInfo.finalEvePrice")
	if err != nil {
		return Batch{}, err
	}
	totalTokensToBurn, err := evmread.BigInt(vals[2], "batchInfo.totalTokensToBurn")
	if err != nil {
		return Batch{}, err
	}
	createdAt, err := evmread.Uint64(vals[3], "batchInfo.createdAt")
	if err != nil {
		return Batch{}, err
	}
	pricedAt, err := evmread.Uint64(vals[4], "batchInfo.pricedAt")
	if err != nil {
		return Batch{}, err
	}

	return Batch{
		ID:                id,
		CanBeProcessed:    canBeProcessed,
		FinalEvePrice:     finalEvePrice,
		TotalTokensToBurn: totalTokensToBurn,
		CreatedAt:         createdAt,
		PricedAt:          pricedAt,
	}, nil
}

// DecodeRequestInfo converts `ExitQueue.requestInfo(uint256,address)`:
//
//	(bool processed, bool closedDueToSlippage, uint256 evePriceAtRequestTime,
//	 uint256 tokensToBurn, uint256 priceTolerance)
func DecodeRequestInfo(user common.Address, vals []any) (Request, error) {
	if len(vals) != 5 {
		return Request{}, fmt.Errorf("queue: requestInfo returned %d values, want 5", len(vals))
	}

	processed, err := evmread.Bool(vals[0], "requestInfo.processed")
	if err != nil {
		return Request{}, err
	}
	closed, err := evmread.Bool(vals[1], "requestInfo.closedDueToSlippage")
	if err != nil {
		return Request{}, err
	}
	priceAtRequest, err := evmread.BigInt(vals[2], "requestInfo.evePriceAtRequestTime")
	if err != nil {
		return Request{}, err
	}
	tokensToBurn, err := evmread.BigInt(vals[3], "requestInfo.tokensToBurn")
	if err != nil {
		return Request{}, err
	}
	tolerance, err := evmread.BigInt(vals[4], "requestInfo.priceTolerance")
	if err != nil {
		return Request{}, err
	}

	return Request{
		User:                  user,
		Processed:             processed,
		ClosedDueToSlippage:   closed,
		EvePriceAtRequestTime: priceAtRequest,
		TokensToBurn:          tokensToBurn,
		PriceTolerance:        tolerance,
	}, nil
}

// DecodeUpkeepStatus converts `CREQueueExecutor.queueUpkeepStatus()`:
//
//	(QueueAction action, uint256 batchId, uint256 count)
//
// The action is a Solidity enum, which the ABI reports as uint8.
func DecodeUpkeepStatus(vals []any) (UpkeepStatus, error) {
	if len(vals) != 3 {
		return UpkeepStatus{}, fmt.Errorf("queue: queueUpkeepStatus returned %d values, want 3", len(vals))
	}

	action, ok := vals[0].(uint8)
	if !ok {
		return UpkeepStatus{}, fmt.Errorf("queue: queueUpkeepStatus action is %T, want uint8", vals[0])
	}
	batchID, err := evmread.Uint64(vals[1], "queueUpkeepStatus.batchId")
	if err != nil {
		return UpkeepStatus{}, err
	}
	count, err := evmread.Uint64(vals[2], "queueUpkeepStatus.count")
	if err != nil {
		return UpkeepStatus{}, err
	}

	return UpkeepStatus{Action: Action(action), BatchID: batchID, Count: count}, nil
}
