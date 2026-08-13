// Package keystone mirrors the workflow-identity metadata the KeystoneForwarder
// delivers alongside a report, and which `CREReceiverBase._validateWorkflowIdentity`
// checks against `expectedWorkflowId` / `expectedAuthor` / `expectedWorkflowName`.
//
// These helpers exist so the binding step of the Sepolia cutover
// (https://github.com/everstrat-xyz/keepers/issues/6) can be computed and
// reviewed from the workflow side, instead of being derived by hand and pasted
// into a setter transaction.
//
// # Wire layout
//
// The receiver copies calldata to memory and reads:
//
//	metadata[0:32]  workflowId   (bytes32)
//	metadata[32:42] workflowName (bytes10)
//	metadata[42:62] workflowOwner (address)
//	metadata[62:]   reportId and any future trailer — ignored
//
// Production deliveries are 64 bytes (62 identity + 2 reportId). The receiver
// requires `length >= 62`, so it tolerates a longer trailer but rejects a
// truncated slice with CREReceiverInvalidMetadata.
package keystone

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

const (
	// IdentityLen is the minimum metadata length the receiver accepts.
	IdentityLen = 62
	// ProductionLen is what the KeystoneForwarder actually delivers:
	// identity plus a 2-byte reportId.
	ProductionLen = 64

	offsetWorkflowID    = 0
	offsetWorkflowName  = 32
	offsetWorkflowOwner = 42
)

// ErrShortMetadata mirrors CREReceiverInvalidMetadata.
var ErrShortMetadata = errors.New("keystone: metadata shorter than 62 bytes")

// Metadata is the decoded workflow identity.
type Metadata struct {
	WorkflowID    common.Hash
	WorkflowName  [10]byte
	WorkflowOwner common.Address
}

// WorkflowNameBytes10 encodes a workflow name the way the CRE engine and
// `CREReceiverBase.setExpectedWorkflowName(string)` do:
//
//	sha256(name) -> lowercase hex string -> first 10 characters -> bytes10
//
// The result is 10 ASCII hex characters, not 10 raw hash bytes. Getting this
// wrong produces a receiver that silently rejects every report, so prefer
// calling `setExpectedWorkflowName(string)` on-chain and using this only to
// predict and verify the stored value.
//
// An empty name maps to the zero value, matching the contract's early return
// that clears the expectation.
func WorkflowNameBytes10(name string) [10]byte {
	var out [10]byte
	if name == "" {
		return out
	}
	sum := sha256.Sum256([]byte(name))
	copy(out[:], hex.EncodeToString(sum[:])[:10])
	return out
}

// WorkflowNameString renders a bytes10 workflow name back to its printable
// form. The bytes are ASCII hex characters, so this is lossless for names
// produced by WorkflowNameBytes10.
func WorkflowNameString(name [10]byte) string {
	return string(name[:])
}

// Encode builds a production-shaped 64-byte metadata blob (identity plus a zero
// reportId). Useful for simulation and for asserting parity with the Solidity
// test helper `CRETestUtils._encodeMetadata`.
func (m Metadata) Encode() []byte {
	out := make([]byte, ProductionLen)
	copy(out[offsetWorkflowID:], m.WorkflowID.Bytes())
	copy(out[offsetWorkflowName:], m.WorkflowName[:])
	copy(out[offsetWorkflowOwner:], m.WorkflowOwner.Bytes())
	return out
}

// Decode parses a metadata blob using the receiver's layout, accepting any
// length the receiver accepts.
func Decode(metadata []byte) (Metadata, error) {
	if len(metadata) < IdentityLen {
		return Metadata{}, fmt.Errorf("%w: got %d", ErrShortMetadata, len(metadata))
	}
	var m Metadata
	m.WorkflowID = common.BytesToHash(metadata[offsetWorkflowID : offsetWorkflowID+32])
	copy(m.WorkflowName[:], metadata[offsetWorkflowName:offsetWorkflowName+10])
	m.WorkflowOwner = common.BytesToAddress(metadata[offsetWorkflowOwner : offsetWorkflowOwner+20])
	return m, nil
}

// Expectations describes what a receiver should be bound to. The zero value of
// each field means "do not check", matching the receiver's setters.
type Expectations struct {
	WorkflowID    common.Hash
	WorkflowName  [10]byte
	WorkflowOwner common.Address
}

// Errors returned by Check and Validate.
var (
	ErrWorkflowIDMismatch     = errors.New("keystone: workflowId does not match expectedWorkflowId")
	ErrAuthorMismatch         = errors.New("keystone: workflowOwner does not match expectedAuthor")
	ErrWorkflowNameMismatch   = errors.New("keystone: workflowName does not match expectedWorkflowName")
	ErrNameRequiresAuthor     = errors.New("keystone: workflow name validation requires author validation (40-bit collision risk)")
	ErrReceiverWouldBeUnbound = errors.New("keystone: receiver needs expectedWorkflowId or expectedAuthor set before it accepts reports")
)

// Validate reports whether a set of expectations is one the receiver will
// actually enforce, applying the two binding rules from `CREReceiverBase`:
//
//  1. Name validation requires author validation. `bytes10` is a 40-bit
//     truncation of a hash; on its own it is not a meaningful authorisation.
//  2. The receiver is inert until `expectedWorkflowId` or `expectedAuthor` is
//     set, so binding only a name leaves the keeper dead.
//
// Run this before submitting the binding transactions, not after.
func (e Expectations) Validate() error {
	if e.WorkflowName != ([10]byte{}) && e.WorkflowOwner == (common.Address{}) {
		return ErrNameRequiresAuthor
	}
	if e.WorkflowID == (common.Hash{}) && e.WorkflowOwner == (common.Address{}) {
		return ErrReceiverWouldBeUnbound
	}
	return nil
}

// Check applies the receiver's identity checks to a decoded metadata blob,
// returning the same failure the contract would revert with.
func (e Expectations) Check(m Metadata) error {
	if e.WorkflowID != (common.Hash{}) && m.WorkflowID != e.WorkflowID {
		return fmt.Errorf("%w: got %s, want %s", ErrWorkflowIDMismatch, m.WorkflowID, e.WorkflowID)
	}
	if e.WorkflowOwner != (common.Address{}) && m.WorkflowOwner != e.WorkflowOwner {
		return fmt.Errorf("%w: got %s, want %s", ErrAuthorMismatch, m.WorkflowOwner, e.WorkflowOwner)
	}
	if e.WorkflowName != ([10]byte{}) {
		if e.WorkflowOwner == (common.Address{}) {
			return ErrNameRequiresAuthor
		}
		if m.WorkflowName != e.WorkflowName {
			return fmt.Errorf("%w: got %q, want %q",
				ErrWorkflowNameMismatch, WorkflowNameString(m.WorkflowName), WorkflowNameString(e.WorkflowName))
		}
	}
	return nil
}
