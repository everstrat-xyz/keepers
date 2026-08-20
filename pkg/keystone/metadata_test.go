package keystone_test

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/everstrat-xyz/keepers/pkg/keystone"
)

// TestWorkflowNameBytes10 pins the encoding against values computed
// independently with `printf '%s' <name> | shasum -a 256 | cut -c1-10`, which
// is the same sha256 -> hex -> first-10-chars pipeline
// CREReceiverBase.setExpectedWorkflowName(string) implements in Solidity.
func TestWorkflowNameBytes10(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"everstrat-queue-keeper-staging", "10c6e65850"},
		{"everstrat-strategy-keeper-staging", "58a4424f9e"},
		{"everstrat-queue-keeper-production", "eb8568b42b"},
		{"fork-meta", "3b33f06205"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keystone.WorkflowNameBytes10(tt.name)
			if keystone.WorkflowNameString(got) != tt.want {
				t.Errorf("WorkflowNameBytes10(%q) = %q, want %q", tt.name, keystone.WorkflowNameString(got), tt.want)
			}
			// The value is ASCII hex characters, not raw hash bytes — the
			// mistake that produces a receiver which rejects everything.
			if got[0] != tt.want[0] {
				t.Errorf("first byte = %#x, want ASCII %q", got[0], tt.want[0])
			}
		})
	}
}

func TestWorkflowNameBytes10Empty(t *testing.T) {
	if got := keystone.WorkflowNameBytes10(""); got != ([10]byte{}) {
		t.Errorf("WorkflowNameBytes10(\"\") = %x, want zero (matches the contract's clear-expectation path)", got)
	}
}

// TestEncodeMatchesSolidityHelper pins the layout against
// CRETestUtils._encodeMetadata: abi.encodePacked(workflowId, workflowName,
// workflowOwner, bytes2(0)).
func TestEncodeMatchesSolidityHelper(t *testing.T) {
	m := keystone.Metadata{
		WorkflowID:    common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111"),
		WorkflowName:  keystone.WorkflowNameBytes10("everstrat-queue-keeper-staging"),
		WorkflowOwner: common.HexToAddress("0x2222222222222222222222222222222222222222"),
	}

	got := m.Encode()
	if len(got) != keystone.ProductionLen {
		t.Fatalf("Encode() length = %d, want %d", len(got), keystone.ProductionLen)
	}

	want := "1111111111111111111111111111111111111111111111111111111111111111" + // workflowId
		hex.EncodeToString([]byte("10c6e65850")) + // workflowName as ASCII hex chars
		"2222222222222222222222222222222222222222" + // workflowOwner
		"0000" // reportId
	if hex.EncodeToString(got) != want {
		t.Errorf("Encode() = %x\nwant       %s", got, want)
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	want := keystone.Metadata{
		WorkflowID:    common.HexToHash("0xabcdef0000000000000000000000000000000000000000000000000000000001"),
		WorkflowName:  keystone.WorkflowNameBytes10("everstrat-strategy-keeper-staging"),
		WorkflowOwner: common.HexToAddress("0x000000000000000000000000000000000000dEaD"),
	}

	got, err := keystone.Decode(want.Encode())
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if got != want {
		t.Errorf("Decode() = %+v, want %+v", got, want)
	}
}

// TestDecodeLengthRules mirrors _validateWorkflowIdentity: reject below 62,
// accept 62 and the 64-byte production slice, and tolerate a longer trailer.
func TestDecodeLengthRules(t *testing.T) {
	full := keystone.Metadata{
		WorkflowID:    common.HexToHash("0x01"),
		WorkflowOwner: common.HexToAddress("0x000000000000000000000000000000000000dEaD"),
	}.Encode()

	tests := []struct {
		name    string
		length  int
		wantErr bool
	}{
		{"61 bytes (the contract's rejected case)", 61, true},
		{"62 bytes (identity only)", 62, false},
		{"64 bytes (production)", 64, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := keystone.Decode(full[:tt.length])
			if tt.wantErr {
				if !errors.Is(err, keystone.ErrShortMetadata) {
					t.Errorf("Decode() error = %v, want %v", err, keystone.ErrShortMetadata)
				}
				return
			}
			if err != nil {
				t.Errorf("Decode() error = %v, want nil", err)
			}
		})
	}

	t.Run("longer trailer is tolerated", func(t *testing.T) {
		padded := append(append([]byte{}, full...), make([]byte, 32)...)
		got, err := keystone.Decode(padded)
		if err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		if got.WorkflowOwner != common.HexToAddress("0x000000000000000000000000000000000000dEaD") {
			t.Errorf("owner = %s, want 0x…dEaD", got.WorkflowOwner)
		}
	})
}

func TestExpectationsValidate(t *testing.T) {
	var (
		id    = common.HexToHash("0x01")
		owner = common.HexToAddress("0x000000000000000000000000000000000000dEaD")
		name  = keystone.WorkflowNameBytes10("everstrat-queue-keeper-staging")
	)

	tests := []struct {
		name string
		exp  keystone.Expectations
		want error
	}{
		{"id only", keystone.Expectations{WorkflowID: id}, nil},
		{"author only", keystone.Expectations{WorkflowOwner: owner}, nil},
		{"author and name", keystone.Expectations{WorkflowOwner: owner, WorkflowName: name}, nil},
		{"all three", keystone.Expectations{WorkflowID: id, WorkflowOwner: owner, WorkflowName: name}, nil},
		{"name without author", keystone.Expectations{WorkflowName: name}, keystone.ErrNameRequiresAuthor},
		{"id and name without author", keystone.Expectations{WorkflowID: id, WorkflowName: name}, keystone.ErrNameRequiresAuthor},
		{"nothing set", keystone.Expectations{}, keystone.ErrReceiverWouldBeUnbound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.exp.Validate()
			switch {
			case tt.want == nil && err != nil:
				t.Errorf("Validate() error = %v, want nil", err)
			case tt.want != nil && !errors.Is(err, tt.want):
				t.Errorf("Validate() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestExpectationsCheck(t *testing.T) {
	var (
		id    = common.HexToHash("0x01")
		owner = common.HexToAddress("0x000000000000000000000000000000000000dEaD")
		name  = keystone.WorkflowNameBytes10("everstrat-queue-keeper-staging")
		other = keystone.WorkflowNameBytes10("everstrat-strategy-keeper-staging")
	)
	bound := keystone.Expectations{WorkflowID: id, WorkflowOwner: owner, WorkflowName: name}
	delivered := keystone.Metadata{WorkflowID: id, WorkflowOwner: owner, WorkflowName: name}

	tests := []struct {
		name string
		exp  keystone.Expectations
		md   keystone.Metadata
		want error
	}{
		{"matching identity", bound, delivered, nil},
		{"unset expectations accept anything", keystone.Expectations{}, delivered, nil},
		{
			"wrong workflow id",
			bound,
			keystone.Metadata{WorkflowID: common.HexToHash("0x02"), WorkflowOwner: owner, WorkflowName: name},
			keystone.ErrWorkflowIDMismatch,
		},
		{
			"wrong author",
			bound,
			keystone.Metadata{WorkflowID: id, WorkflowOwner: common.HexToAddress("0x01"), WorkflowName: name},
			keystone.ErrAuthorMismatch,
		},
		{
			"wrong name",
			bound,
			keystone.Metadata{WorkflowID: id, WorkflowOwner: owner, WorkflowName: other},
			keystone.ErrWorkflowNameMismatch,
		},
		{
			"name checked without author binding",
			keystone.Expectations{WorkflowName: name},
			delivered,
			keystone.ErrNameRequiresAuthor,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.exp.Check(tt.md)
			switch {
			case tt.want == nil && err != nil:
				t.Errorf("Check() error = %v, want nil", err)
			case tt.want != nil && !errors.Is(err, tt.want):
				t.Errorf("Check() error = %v, want %v", err, tt.want)
			}
		})
	}
}
