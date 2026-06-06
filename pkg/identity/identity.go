// Package identity provides explicit, mutually-distinct identity types for
// OpenRails billing.
package identity

import (
	"strings"

	"github.com/google/uuid"
)

// TenantSubjectID identifies a billing.tenant_subjects row: the OpenRails
// payable subject whose balance, invoices, reservations, and entitlements are
// recorded. It is distinct from any invoker or operator identity so the compiler
// rejects passing the wrong one.
type TenantSubjectID uuid.UUID

// InvokerID is the user / service token / delegated principal that invoked
// usage. It is attribution + budgeting, never ownership.
type InvokerID string

// String returns the canonical string form of the tenant subject id.
func (id TenantSubjectID) String() string { return uuid.UUID(id).String() }

// UUID returns the underlying uuid.UUID for use in queries.
func (id TenantSubjectID) UUID() uuid.UUID { return uuid.UUID(id) }

// IsZero reports whether the tenant subject id is unset.
func (id TenantSubjectID) IsZero() bool { return uuid.UUID(id) == uuid.Nil }

// String returns the invoker id as a plain string.
func (a InvokerID) String() string { return string(a) }

// IsZero reports whether the invoker id is empty.
func (a InvokerID) IsZero() bool { return strings.TrimSpace(string(a)) == "" }

// TenantSubjectIDFromString parses s as a tenant subject id. Empty or non-UUID
// input yields the zero TenantSubjectID, which callers must reject.
func TenantSubjectIDFromString(s string) TenantSubjectID {
	s = strings.TrimSpace(s)
	if s == "" {
		return TenantSubjectID(uuid.Nil)
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return TenantSubjectID(uuid.Nil)
	}
	return TenantSubjectID(id)
}
