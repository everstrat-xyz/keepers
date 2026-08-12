package evmread_test

import (
	"testing"

	everabi "github.com/everstrat-xyz/keepers/contracts/evm/src/abi"
	"github.com/everstrat-xyz/keepers/pkg/evmread"
)

// TestLimitsMatchCRE pins the constants against `cre workflow limits export`.
// They are not tuning knobs: exceeding CallLimit aborts the whole execution
// with Public:User:LimitExceeded, which is how the batching in this package
// came to exist.
func TestLimitsMatchCRE(t *testing.T) {
	if evmread.MaxChainReads != 15 {
		t.Errorf("MaxChainReads = %d, want 15 (ChainRead.CallLimit)", evmread.MaxChainReads)
	}
	if evmread.MaxReadPayloadBytes != 5*1024 {
		t.Errorf("MaxReadPayloadBytes = %d, want 5120 (ChainRead.PayloadSizeLimit)", evmread.MaxReadPayloadBytes)
	}
}

func TestMulticall3AddressIsCanonical(t *testing.T) {
	// Verified live on Ethereum mainnet and Sepolia (3808 bytes of code).
	if got, want := evmread.Multicall3Address.Hex(), "0xcA11bde05977b3631167028862bE2a173976CA11"; got != want {
		t.Errorf("Multicall3Address = %s, want %s", got, want)
	}
}

func TestAggregate3ABIShape(t *testing.T) {
	m, err := everabi.Method(everabi.Multicall3, "aggregate3")
	if err != nil {
		t.Fatal(err)
	}
	if want := "aggregate3((address,bool,bytes)[])"; m.Sig != want {
		t.Errorf("aggregate3 signature = %q, want %q", m.Sig, want)
	}
	if len(m.Outputs) != 1 {
		t.Fatalf("aggregate3 returns %d values, want 1", len(m.Outputs))
	}
	if got, want := m.Outputs[0].Type.String(), "(bool,bytes)[]"; got != want {
		t.Errorf("aggregate3 return type = %s, want %s", got, want)
	}
}

func TestBudget(t *testing.T) {
	b := evmread.NewBudget(2)
	if got := b.Remaining(); got != evmread.MaxChainReads-2 {
		t.Fatalf("Remaining() = %d, want %d", got, evmread.MaxChainReads-2)
	}

	if !b.Take(3) {
		t.Error("Take(3) = false, want true")
	}
	if got := b.Remaining(); got != 10 {
		t.Errorf("Remaining() after Take(3) = %d, want 10", got)
	}

	if b.Take(11) {
		t.Error("Take(11) = true with only 10 left; over-taking is what aborts the execution")
	}
	if got := b.Remaining(); got != 10 {
		t.Errorf("a refused Take must not consume budget; Remaining() = %d, want 10", got)
	}

	if got := b.TakeUpTo(50); got != 10 {
		t.Errorf("TakeUpTo(50) = %d, want 10", got)
	}
	if got := b.Remaining(); got != 0 {
		t.Errorf("Remaining() = %d, want 0", got)
	}
	if b.Take(1) {
		t.Error("Take(1) = true on an exhausted budget")
	}
}

func TestBudgetOversizeReserve(t *testing.T) {
	// A reserve larger than the limit must clamp to zero rather than go
	// negative and wrap into a huge allowance.
	b := evmread.NewBudget(evmread.MaxChainReads + 5)
	if got := b.Remaining(); got != 0 {
		t.Errorf("Remaining() = %d, want 0", got)
	}
}

func TestEstimateResultBytes(t *testing.T) {
	// A 160-byte return (batchInfo, requestInfo) plus framing.
	if got := evmread.EstimateResultBytes(160); got != 5*32+160 {
		t.Errorf("EstimateResultBytes(160) = %d, want %d", got, 5*32+160)
	}
	// Non-multiples of 32 round up to a whole word.
	if got, want := evmread.EstimateResultBytes(33), 5*32+64; got != want {
		t.Errorf("EstimateResultBytes(33) = %d, want %d", got, want)
	}
}

func TestChunkSubCalls(t *testing.T) {
	calls := make([]evmread.SubCall, 40)

	// 160-byte returns: 320 bytes each, so 16 fit in the 5kb payload cap.
	chunks := evmread.ChunkSubCalls(calls, evmread.EstimateResultBytes(160))
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	total := 0
	for i, c := range chunks {
		if len(c) > 16 {
			t.Errorf("chunk %d has %d calls; a chunk over the payload cap fails the whole read", i, len(c))
		}
		total += len(c)
	}
	if total != len(calls) {
		t.Errorf("chunks cover %d calls, want %d", total, len(calls))
	}
}

func TestChunkSubCallsEdges(t *testing.T) {
	if got := evmread.ChunkSubCalls(nil, 100); got != nil {
		t.Errorf("ChunkSubCalls(nil) = %v, want nil", got)
	}

	// A return so large that not even one result fits must still make
	// progress, one call per batch, rather than loop forever on a zero size.
	chunks := evmread.ChunkSubCalls(make([]evmread.SubCall, 3), evmread.MaxReadPayloadBytes*2)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3 (one per call)", len(chunks))
	}
	for i, c := range chunks {
		if len(c) != 1 {
			t.Errorf("chunk %d has %d calls, want 1", i, len(c))
		}
	}
}

// TestReadPlanFitsBudget is the arithmetic that keeps W1 inside the limit: the
// fixed preamble plus the cross-check must leave room for a scan meaningfully
// deeper than the on-chain view's 25-batch window.
func TestReadPlanFitsBudget(t *testing.T) {
	const (
		preambleReads   = 3 // 2 multicall rounds + block timestamp
		balanceRead     = 1
		crossCheckRead  = 1
		batchSubCalls   = 2 // batchInfo + unprocessedUsersCount
		perMulticallCap = evmread.MaxReadPayloadBytes / (5*32 + 160)
	)

	scanReads := evmread.MaxChainReads - preambleReads - balanceRead - crossCheckRead
	if scanReads < 1 {
		t.Fatalf("no reads left for the scan (%d)", scanReads)
	}

	// Reserve two reads for the user-list and request-detail phases.
	batchScanReads := scanReads - 2
	batchesReachable := batchScanReads * perMulticallCap / batchSubCalls

	if batchesReachable <= 25 {
		t.Errorf("reachable scan depth is %d batches, which is no better than the on-chain 25-batch window",
			batchesReachable)
	}
	t.Logf("read plan reaches ~%d batches per tick versus the on-chain window of 25", batchesReachable)
}
