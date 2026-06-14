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
