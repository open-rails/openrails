// Package identity provides explicit, mutually-distinct identity types for
// OpenRails openrails.
package identity

import (
	"strings"

	"github.com/google/uuid"
)

// MerchantSubjectID identifies a openrails.tenant_subjects row: the OpenRails
// payable subject whose balance, invoices, reservations, and entitlements are
// recorded. It is distinct from any actor or operator identity so the compiler
// rejects passing the wrong one.
type MerchantSubjectID uuid.UUID

// Actor is the user / service token / delegated principal that invoked
// usage. It is attribution + budgeting, never ownership.
type Actor string

// String returns the canonical string form of the tenant subject id.
func (id MerchantSubjectID) String() string { return uuid.UUID(id).String() }

// UUID returns the underlying uuid.UUID for use in queries.
func (id MerchantSubjectID) UUID() uuid.UUID { return uuid.UUID(id) }

// IsZero reports whether the tenant subject id is unset.
func (id MerchantSubjectID) IsZero() bool { return uuid.UUID(id) == uuid.Nil }

// String returns the actor id as a plain string.
func (a Actor) String() string { return string(a) }

// IsZero reports whether the actor id is empty.
func (a Actor) IsZero() bool { return strings.TrimSpace(string(a)) == "" }

// MerchantSubjectIDFromString parses s as a tenant subject id. Empty or non-UUID
// input yields the zero MerchantSubjectID, which callers must reject.
func MerchantSubjectIDFromString(s string) MerchantSubjectID {
	s = strings.TrimSpace(s)
	if s == "" {
		return MerchantSubjectID(uuid.Nil)
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return MerchantSubjectID(uuid.Nil)
	}
	return MerchantSubjectID(id)
}
