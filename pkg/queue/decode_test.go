package queue_test

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	everabi "github.com/everstrat-xyz/keepers/contracts/evm/src/abi"
	"github.com/everstrat-xyz/keepers/pkg/queue"
)

// unpack decodes return data using the real vendored ABI, so these tests
// exercise the same path the workflow does — including go-ethereum's choice of
// Go types per Solidity type, which is where the decode helpers earn their
// keep.
func unpack(t *testing.T, name everabi.Name, method, data string) []any {
	t.Helper()
	m, err := everabi.Method(name, method)
	if err != nil {
		t.Fatalf("looking up %s.%s: %v", name, method, err)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(data, "0x"))
	if err != nil {
		t.Fatalf("decoding hex: %v", err)
	}
	vals, err := m.Outputs.Unpack(raw)
	if err != nil {
		t.Fatalf("unpacking %s.%s: %v", name, method, err)
	}
	return vals
}

// Return data generated with:
//
//	cast abi-encode "f(bool,uint256,uint256,uint256,uint256)" true 1e18 5e18 1700000000 1700003600
const batchInfoReturn = "0x" +
	"0000000000000000000000000000000000000000000000000000000000000001" + // canBeProcessed
	"0000000000000000000000000000000000000000000000000de0b6b3a7640000" + // finalEvePrice 1e18
	"0000000000000000000000000000000000000000000000004563918244f40000" + // totalTokensToBurn 5e18
	"000000000000000000000000000000000000000000000000000000006553f100" + // createdAt
	"000000000000000000000000000000000000000000000000000000006553ff10" //  pricedAt

func TestDecodeBatchInfo(t *testing.T) {
	vals := unpack(t, everabi.IExitQueue, "batchInfo", batchInfoReturn)

	got, err := queue.DecodeBatchInfo(7, vals)
	if err != nil {
		t.Fatalf("DecodeBatchInfo() error = %v", err)
	}

	if got.ID != 7 {
		t.Errorf("ID = %d, want 7", got.ID)
	}
	if !got.CanBeProcessed {
		t.Error("CanBeProcessed = false, want true")
	}
	if want := big.NewInt(1e18); got.FinalEvePrice.Cmp(want) != 0 {
		t.Errorf("FinalEvePrice = %s, want %s", got.FinalEvePrice, want)
	}
	if want := new(big.Int).Mul(big.NewInt(5), big.NewInt(1e18)); got.TotalTokensToBurn.Cmp(want) != 0 {
		t.Errorf("TotalTokensToBurn = %s, want %s", got.TotalTokensToBurn, want)
	}
	if got.CreatedAt != 1700000000 {
		t.Errorf("CreatedAt = %d, want 1700000000", got.CreatedAt)
	}
	if got.PricedAt != 1700003600 {
		t.Errorf("PricedAt = %d, want 1700003600", got.PricedAt)
	}
}

// Return data generated with:
//
//	cast abi-encode "f(bool,bool,uint256,uint256,uint256)" false false 1.05e18 2e18 5e16
const requestInfoReturn = "0x" +
	"0000000000000000000000000000000000000000000000000000000000000000" + // processed
	"0000000000000000000000000000000000000000000000000000000000000000" + // closedDueToSlippage
	"0000000000000000000000000000000000000000000000000e92596fd6290000" + // evePriceAtRequestTime 1.05e18
	"0000000000000000000000000000000000000000000000001bc16d674ec80000" + // tokensToBurn 2e18
	"00000000000000000000000000000000000000000000000000b1a2bc2ec50000" //  priceTolerance 5e16

func TestDecodeRequestInfo(t *testing.T) {
	vals := unpack(t, everabi.IExitQueue, "requestInfo", requestInfoReturn)

	addr := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	got, err := queue.DecodeRequestInfo(addr, vals)
	if err != nil {
		t.Fatalf("DecodeRequestInfo() error = %v", err)
	}

	if got.User != addr {
		t.Errorf("User = %s, want %s", got.User, addr)
	}
	if got.Processed || got.ClosedDueToSlippage {
		t.Errorf("Processed/ClosedDueToSlippage = %v/%v, want false/false", got.Processed, got.ClosedDueToSlippage)
	}
	if want, _ := new(big.Int).SetString("1050000000000000000", 10); got.EvePriceAtRequestTime.Cmp(want) != 0 {
		t.Errorf("EvePriceAtRequestTime = %s, want %s", got.EvePriceAtRequestTime, want)
	}
	if want, _ := new(big.Int).SetString("2000000000000000000", 10); got.TokensToBurn.Cmp(want) != 0 {
		t.Errorf("TokensToBurn = %s, want %s", got.TokensToBurn, want)
	}
	if want, _ := new(big.Int).SetString("50000000000000000", 10); got.PriceTolerance.Cmp(want) != 0 {
		t.Errorf("PriceTolerance = %s, want %s", got.PriceTolerance, want)
	}
}

// TestDecodeRequestInfoFeedsTheCostModel closes the loop from raw chain bytes
// to an affordability decision, which is the path a misdecoded field would
// corrupt silently.
func TestDecodeRequestInfoFeedsTheCostModel(t *testing.T) {
	vals := unpack(t, everabi.IExitQueue, "requestInfo", requestInfoReturn)
	r, err := queue.DecodeRequestInfo(common.Address{}, vals)
	if err != nil {
		t.Fatal(err)
	}

	// Batch priced at 1e18 against a request queued at 1.05e18 — a 4.76% drop,
	// inside the 5% tolerance, so the request costs full price.
	cost, err := queue.RequestCost(big.NewInt(1e18), r)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := new(big.Int).SetString("2000000000000000000", 10)
	if cost.Cmp(want) != 0 {
		t.Errorf("RequestCost = %s, want %s (2 EVE at 1 ETH)", cost, want)
	}

	// Drop the batch price to 0.9e18 — now past tolerance, so it closes free.
	cost, err = queue.RequestCost(wei("900000000000000000"), r)
	if err != nil {
		t.Fatal(err)
	}
	if cost.Sign() != 0 {
		t.Errorf("RequestCost past tolerance = %s, want 0", cost)
	}
}

// Return data generated with: cast abi-encode "f(uint8,uint256,uint256)" 2 42 5
const upkeepStatusReturn = "0x" +
	"0000000000000000000000000000000000000000000000000000000000000002" +
	"000000000000000000000000000000000000000000000000000000000000002a" +
	"0000000000000000000000000000000000000000000000000000000000000005"

func TestDecodeUpkeepStatus(t *testing.T) {
	vals := unpack(t, everabi.ICREQueueExecutor, "queueUpkeepStatus", upkeepStatusReturn)

	got, err := queue.DecodeUpkeepStatus(vals)
	if err != nil {
		t.Fatalf("DecodeUpkeepStatus() error = %v", err)
	}
	if got.Action != queue.ActionProcessRequests {
		t.Errorf("Action = %s, want ProcessRequests", got.Action)
	}
	if got.BatchID != 42 || got.Count != 5 {
		t.Errorf("got batch %d count %d, want 42 and 5", got.BatchID, got.Count)
	}
}

func TestDecodeRejectsWrongArity(t *testing.T) {
	if _, err := queue.DecodeBatchInfo(1, []any{true}); err == nil {
		t.Error("DecodeBatchInfo() accepted 1 value, want error")
	}
	if _, err := queue.DecodeRequestInfo(common.Address{}, nil); err == nil {
		t.Error("DecodeRequestInfo() accepted 0 values, want error")
	}
	if _, err := queue.DecodeUpkeepStatus([]any{uint8(1)}); err == nil {
		t.Error("DecodeUpkeepStatus() accepted 1 value, want error")
	}
}

func TestDecodeRejectsWrongTypes(t *testing.T) {
	// The realistic failure: a contract-side type change makes a field decode
	// as a different Go type, which a bare assertion would panic on.
	vals := []any{true, big.NewInt(1), big.NewInt(1), big.NewInt(1), "not a big int"}
	if _, err := queue.DecodeBatchInfo(1, vals); err == nil {
		t.Error("DecodeBatchInfo() accepted a string pricedAt, want error")
	}
}

// TestUnprocessedUsersOverloadName pins go-ethereum's overload renaming.
// ExitQueue declares unprocessedUsers twice; the parser keeps the first as
// `unprocessedUsers` and renames the second to `unprocessedUsers0`. The reads
// layer needs the three-argument form, and picking up the one-argument form by
// accident would fetch every user in the batch instead of a bounded prefix.
func TestUnprocessedUsersOverloadName(t *testing.T) {
	m, err := everabi.Method(everabi.IExitQueue, "unprocessedUsers")
	if err != nil {
		t.Fatal(err)
	}
	if want := "unprocessedUsers(uint256,uint256,uint256)"; m.Sig != want {
		t.Errorf("unprocessedUsers resolves to %q, want %q — the reads layer depends on the bounded form", m.Sig, want)
	}
}
