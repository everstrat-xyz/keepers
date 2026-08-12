package queue_test

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/everstrat-xyz/keepers/pkg/envelope"
	"github.com/everstrat-xyz/keepers/pkg/queue"
)

const sepolia = uint64(16015286601757825753)

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatalf("decoding %q: %v", s, err)
	}
	return b
}

// TestActionOrdinalsMatchSolidity pins the enum against
// ICREQueueExecutor.QueueAction. Reordering the Solidity enum without updating
// these values would silently retarget every report at the wrong action.
func TestActionOrdinalsMatchSolidity(t *testing.T) {
	for _, tt := range []struct {
		action queue.Action
		want   uint8
		name   string
	}{
		{queue.ActionNone, 0, "None"},
		{queue.ActionPriceBatch, 1, "PriceBatch"},
		{queue.ActionProcessRequests, 2, "ProcessRequests"},
		{queue.ActionAdvanceCursor, 3, "AdvanceCursor"},
	} {
		if uint8(tt.action) != tt.want {
			t.Errorf("%s = %d, want %d", tt.name, uint8(tt.action), tt.want)
		}
		if tt.action.String() != tt.name {
			t.Errorf("String() = %q, want %q", tt.action.String(), tt.name)
		}
	}

	if queue.ActionNone.Valid() {
		t.Error("ActionNone.Valid() = true; None is not a deliverable action")
	}
	if queue.Action(4).Valid() {
		t.Error("Action(4).Valid() = true; there is no fourth QueueAction")
	}
}

// Golden params bytes from `cast abi-encode "f(uint256)" 7` and
// `cast abi-encode "f(uint256,uint256,uint256)" 42 0 5`.
func TestParamsEncodingMatchesSolidity(t *testing.T) {
	tests := []struct {
		name string
		got  func() ([]byte, error)
		want string
	}{
		{
			name: "PriceBatch",
			got:  func() ([]byte, error) { return queue.EncodePriceBatchParams(7) },
			want: "0x0000000000000000000000000000000000000000000000000000000000000007",
		},
		{
			name: "AdvanceCursor",
			got:  func() ([]byte, error) { return queue.EncodeAdvanceCursorParams(9) },
			want: "0x0000000000000000000000000000000000000000000000000000000000000009",
		},
		{
			name: "ProcessRequests",
			got:  func() ([]byte, error) { return queue.EncodeProcessRequestsParams(42, 5) },
			want: "0x000000000000000000000000000000000000000000000000000000000000002a" +
				"0000000000000000000000000000000000000000000000000000000000000000" +
				"0000000000000000000000000000000000000000000000000000000000000005",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.got()
			if err != nil {
				t.Fatalf("encode error = %v", err)
			}
			if "0x"+hex.EncodeToString(got) != tt.want {
				t.Errorf("params = 0x%x, want %s", got, tt.want)
			}
		})
	}
}

func TestEncodeProcessRequestsParamsRejectsEmptyRange(t *testing.T) {
	if _, err := queue.EncodeProcessRequestsParams(1, 0); !errors.Is(err, queue.ErrEmptyRange) {
		t.Errorf("error = %v, want %v", err, queue.ErrEmptyRange)
	}
}

func TestDecodeParamsRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		action queue.Action
		encode func() ([]byte, error)
		want   queue.Params
	}{
		{
			name:   "PriceBatch",
			action: queue.ActionPriceBatch,
			encode: func() ([]byte, error) { return queue.EncodePriceBatchParams(7) },
			want:   queue.Params{Action: queue.ActionPriceBatch, BatchID: 7},
		},
		{
			name:   "AdvanceCursor",
			action: queue.ActionAdvanceCursor,
			encode: func() ([]byte, error) { return queue.EncodeAdvanceCursorParams(9) },
			want:   queue.Params{Action: queue.ActionAdvanceCursor, BatchID: 9},
		},
		{
			name:   "ProcessRequests",
			action: queue.ActionProcessRequests,
			encode: func() ([]byte, error) { return queue.EncodeProcessRequestsParams(42, 5) },
			want:   queue.Params{Action: queue.ActionProcessRequests, BatchID: 42, StartIndex: 0, EndIndex: 5},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params, err := tt.encode()
			if err != nil {
				t.Fatalf("encode error = %v", err)
			}
			got, err := queue.DecodeParams(tt.action, params)
			if err != nil {
				t.Fatalf("DecodeParams() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("DecodeParams() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestDecodeParamsRejectsSmuggledAmounts is the hard-constraint test: a report
// must never carry an authoritative ETH amount. The params layouts are static,
// so an amount can only ride along as an extra trailing word — which Solidity's
// abi.decode would happily ignore, and which this rejects.
func TestDecodeParamsRejectsSmuggledAmounts(t *testing.T) {
	oneETH := "0000000000000000000000000000000000000000000000000de0b6b3a7640000"

	tests := []struct {
		name   string
		action queue.Action
		params []byte
	}{
		{
			name:   "amount appended to PriceBatch params",
			action: queue.ActionPriceBatch,
			params: hexBytes(t, "0000000000000000000000000000000000000000000000000000000000000007"+oneETH),
		},
		{
			name:   "amount appended to AdvanceCursor params",
			action: queue.ActionAdvanceCursor,
			params: hexBytes(t, "0000000000000000000000000000000000000000000000000000000000000009"+oneETH),
		},
		{
			name:   "amount appended to ProcessRequests params",
			action: queue.ActionProcessRequests,
			params: hexBytes(t,
				"000000000000000000000000000000000000000000000000000000000000002a"+
					"0000000000000000000000000000000000000000000000000000000000000000"+
					"0000000000000000000000000000000000000000000000000000000000000005"+
					oneETH),
		},
		{
			name:   "NAV word prepended, shifting every field",
			action: queue.ActionPriceBatch,
			params: hexBytes(t, oneETH+"0000000000000000000000000000000000000000000000000000000000000007"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := queue.DecodeParams(tt.action, tt.params); !errors.Is(err, queue.ErrParamsLength) {
				t.Errorf("DecodeParams() error = %v, want %v", err, queue.ErrParamsLength)
			}
		})
	}
}

func TestDecodeParamsRejectsMalformedInput(t *testing.T) {
	valid := hexBytes(t, "0000000000000000000000000000000000000000000000000000000000000007")

	t.Run("truncated", func(t *testing.T) {
		if _, err := queue.DecodeParams(queue.ActionPriceBatch, valid[:16]); !errors.Is(err, queue.ErrParamsLength) {
			t.Errorf("error = %v, want %v", err, queue.ErrParamsLength)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := queue.DecodeParams(queue.ActionPriceBatch, nil); !errors.Is(err, queue.ErrParamsLength) {
			t.Errorf("error = %v, want %v", err, queue.ErrParamsLength)
		}
	})
	t.Run("non-actionable action", func(t *testing.T) {
		if _, err := queue.DecodeParams(queue.ActionNone, valid); !errors.Is(err, queue.ErrUnknownAction) {
			t.Errorf("error = %v, want %v", err, queue.ErrUnknownAction)
		}
	})
	t.Run("startIndex not zero", func(t *testing.T) {
		params := hexBytes(t,
			"000000000000000000000000000000000000000000000000000000000000002a"+
				"0000000000000000000000000000000000000000000000000000000000000001"+
				"0000000000000000000000000000000000000000000000000000000000000005")
		if _, err := queue.DecodeParams(queue.ActionProcessRequests, params); !errors.Is(err, queue.ErrStartIndexNotZero) {
			t.Errorf("error = %v, want %v", err, queue.ErrStartIndexNotZero)
		}
	})
	t.Run("batch id overflows uint64", func(t *testing.T) {
		params := hexBytes(t, "0000000000000000000000000000000000000000000000010000000000000000")
		if _, err := queue.DecodeParams(queue.ActionPriceBatch, params); err == nil {
			t.Error("DecodeParams() succeeded on a uint64 overflow, want error")
		}
	})
}

func TestReportBuildersProduceDecodableEnvelopes(t *testing.T) {
	r := queue.Report{ChainSelector: sepolia, Sequence: 11, ObservedAt: 1700000000}

	tests := []struct {
		name       string
		build      func() ([]byte, error)
		wantAction queue.Action
		wantParams queue.Params
	}{
		{
			name:       "PriceBatch",
			build:      func() ([]byte, error) { return r.PriceBatch(7) },
			wantAction: queue.ActionPriceBatch,
			wantParams: queue.Params{Action: queue.ActionPriceBatch, BatchID: 7},
		},
		{
			name:       "ProcessRequests",
			build:      func() ([]byte, error) { return r.ProcessRequests(42, 5) },
			wantAction: queue.ActionProcessRequests,
			wantParams: queue.Params{Action: queue.ActionProcessRequests, BatchID: 42, EndIndex: 5},
		},
		{
			name:       "AdvanceCursor",
			build:      func() ([]byte, error) { return r.AdvanceCursor(9) },
			wantAction: queue.ActionAdvanceCursor,
			wantParams: queue.Params{Action: queue.ActionAdvanceCursor, BatchID: 9},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := tt.build()
			if err != nil {
				t.Fatalf("build error = %v", err)
			}
			e, err := envelope.Decode(report)
			if err != nil {
				t.Fatalf("envelope.Decode() error = %v", err)
			}
			if e.ChainSelector != sepolia || e.Sequence != 11 || e.ObservedAt != 1700000000 {
				t.Errorf("header = %+v, want selector %d seq 11 observedAt 1700000000", e, sepolia)
			}
			if queue.Action(e.Action) != tt.wantAction {
				t.Errorf("action = %s, want %s", queue.Action(e.Action), tt.wantAction)
			}
			got, err := queue.DecodeParams(queue.Action(e.Action), e.Params)
			if err != nil {
				t.Fatalf("DecodeParams() error = %v", err)
			}
			if got != tt.wantParams {
				t.Errorf("params = %+v, want %+v", got, tt.wantParams)
			}
		})
	}
}

// TestReportMatchesEnvelopeFixture pins a full W1 report against the Solidity
// golden bytes in pkg/envelope/testdata (fixture "queue_process_requests").
func TestReportMatchesEnvelopeFixture(t *testing.T) {
	const want = "0x0000000000000000000000000000000000000000000000000000000000000020" +
		"000000000000000000000000000000000000000000000000de41ba4fc9d91ad9" +
		"0000000000000000000000000000000000000000000000000000000000000002" +
		"000000000000000000000000000000000000000000000000000000006553f13c" +
		"0000000000000000000000000000000000000000000000000000000000000002" +
		"00000000000000000000000000000000000000000000000000000000000000a0" +
		"0000000000000000000000000000000000000000000000000000000000000060" +
		"000000000000000000000000000000000000000000000000000000000000002a" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000005"

	r := queue.Report{ChainSelector: sepolia, Sequence: 2, ObservedAt: 1700000060}
	got, err := r.ProcessRequests(42, 5)
	if err != nil {
		t.Fatalf("ProcessRequests() error = %v", err)
	}
	if "0x"+hex.EncodeToString(got) != want {
		t.Errorf("report = 0x%x\nwant    %s", got, want)
	}
}

func TestReportRejectsBadHeaders(t *testing.T) {
	r := queue.Report{ChainSelector: 0, Sequence: 1, ObservedAt: 1700000000}
	if _, err := r.PriceBatch(1); !errors.Is(err, envelope.ErrZeroChainSelector) {
		t.Errorf("error = %v, want %v", err, envelope.ErrZeroChainSelector)
	}
}
