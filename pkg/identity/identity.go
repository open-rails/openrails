// Package identity provides explicit, mutually-distinct identity types for
// OpenRails openrails.
package identity

import (
	"strings"

	"github.com/google/uuid"
)

// CustomerID identifies an openrails.customers row: the OpenRails
// payable subject whose balance, invoices, reservations, and entitlements are
// recorded. It is distinct from any actor or operator identity so the compiler
// rejects passing the wrong one.
type CustomerID uuid.UUID

// Actor is the user / service token / delegated principal that invoked
// usage. It is attribution + budgeting, never ownership.
type Actor string

// String returns the canonical string form of the customer id.
func (id CustomerID) String() string { return uuid.UUID(id).String() }

// UUID returns the underlying uuid.UUID for use in queries.
func (id CustomerID) UUID() uuid.UUID { return uuid.UUID(id) }

// IsZero reports whether the customer id is unset.
func (id CustomerID) IsZero() bool { return uuid.UUID(id) == uuid.Nil }

// String returns the actor id as a plain string.
func (a Actor) String() string { return string(a) }

// IsZero reports whether the actor id is empty.
func (a Actor) IsZero() bool { return strings.TrimSpace(string(a)) == "" }

// federatedNamespace is the fixed UUIDv5 namespace for deriving a customer id
// from a federated (merchant, issuer, subject) triple (#491). Fixed forever:
// changing it would re-key every federated payer.
var federatedNamespace = uuid.MustParse("a3f1e2c4-0b6d-5a47-9c8e-2f1b3d4e5a60")

// FederatedCustomerID derives the deterministic payable customer id for a
// federated (issuer, subject) pair under a merchant (#491). customers is now
// UUID-only (no (issuer, subject) lookup column); the same triple always maps to
// the same UUID, so federated resolution stays idempotent without a lookup.
func FederatedCustomerID(merchantID uuid.UUID, issuer, subject string) CustomerID {
	name := merchantID.String() + "\x00" + strings.TrimSpace(issuer) + "\x00" + strings.TrimSpace(subject)
	return CustomerID(uuid.NewSHA1(federatedNamespace, []byte(name)))
}

// CustomerIDFromString parses s as a customer id. Empty or non-UUID
// input yields the zero CustomerID, which callers must reject.
func CustomerIDFromString(s string) CustomerID {
	s = strings.TrimSpace(s)
	if s == "" {
		return CustomerID(uuid.Nil)
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return CustomerID(uuid.Nil)
	}
	return CustomerID(id)
}
