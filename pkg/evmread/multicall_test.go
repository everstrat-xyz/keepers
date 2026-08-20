package evmread_test

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

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

// TestSubResultAccessorsRejectWrongArity is why these accessors exist rather
// than a bare type assertion at each read site: a sub-call whose ABI changed
// shape comes back with a different number of values, and indexing Values[0]
// blindly would panic inside the workflow instead of failing the read with a
// message naming the field.
func TestSubResultAccessorsRejectWrongArity(t *testing.T) {
	empty := evmread.SubResult{Success: true}
	if _, err := empty.Uint64("currentBatchId"); err == nil {
		t.Error("Uint64 on a zero-value result = nil error, want an arity error")
	}
	if _, err := empty.Bool("paused"); err == nil {
		t.Error("Bool on a zero-value result = nil error, want an arity error")
	}
	if _, err := empty.BigInt("freeBalance"); err == nil {
		t.Error("BigInt on a zero-value result = nil error, want an arity error")
	}
	if _, err := empty.Address("controller"); err == nil {
		t.Error("Address on a zero-value result = nil error, want an arity error")
	}

	twoValues := evmread.SubResult{Success: true, Values: []any{uint64(1), uint64(2)}}
	if _, err := twoValues.Uint64("batchInfo"); err == nil {
		t.Error("Uint64 on a 2-value result = nil error, want an arity error")
	}
}

// TestSubResultAccessorsRejectWrongTypes and the overflow case below are the
// silent-corruption traps the accessors close: a value that arrives in the
// wrong Go shape means the ABI and the reader disagree, and a uint256 that
// does not fit uint64 would otherwise truncate into a batch id or timestamp.
// Every error has to name the field, or a failure points at sub-call index N.
func TestSubResultAccessorsRejectWrongTypes(t *testing.T) {
	if _, err := (evmread.SubResult{Success: true, Values: []any{true}}).Uint64("cursor"); err == nil {
		t.Error("Uint64 on a bool = nil error, want a type error naming cursor")
	}
	if _, err := (evmread.SubResult{Success: true, Values: []any{big.NewInt(1)}}).Bool("receiver.paused"); err == nil {
		t.Error("Bool on an integer = nil error, want a type error naming receiver.paused")
	}
	if _, err := (evmread.SubResult{Success: true, Values: []any{false}}).BigInt("controllerBalance"); err == nil {
		t.Error("BigInt on a bool = nil error, want a type error naming controllerBalance")
	}
	if _, err := (evmread.SubResult{Success: true,
		Values: []any{"0xcA11bde05977b3631167028862bE2a173976CA11"}}).Address("exitQueue"); err == nil {
		t.Error("Address on a string = nil error, want a type error naming exitQueue")
	}
}

func TestSubResultUint64RejectsOverflow(t *testing.T) {
	overflow := evmread.SubResult{Success: true, Values: []any{new(big.Int).Lsh(big.NewInt(1), 64)}}
	if _, err := overflow.Uint64("cursor"); err == nil {
		t.Error("Uint64 on 2^64 = nil error, want an overflow error; truncating would corrupt a batch id")
	}
}

func TestSubResultAccessorsDecodeSingleValues(t *testing.T) {
	got, err := evmread.SubResult{Success: true, Values: []any{big.NewInt(7)}}.Uint64("currentBatchId")
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Errorf("Uint64 = %d, want 7", got)
	}

	paused, err := evmread.SubResult{Success: true, Values: []any{true}}.Bool("paused")
	if err != nil {
		t.Fatal(err)
	}
	if !paused {
		t.Error("Bool = false, want true")
	}

	addr, err := evmread.SubResult{
		Success: true,
		Values:  []any{common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11")},
	}.Address("controller")
	if err != nil {
		t.Fatal(err)
	}
	if addr != common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11") {
		t.Errorf("Address = %s, want the Multicall3 address", addr)
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
