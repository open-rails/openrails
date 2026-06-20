// Package identity provides explicit, mutually-distinct identity types for
// OpenRails openrails.
package identity

import (
	"strings"

	"github.com/google/uuid"
)

// CustomerID identifies an openrails.customers row: the OpenRails
// payable subject whose balance, invoices, reservations, and entitlements are
// recorded. It is distinct from any invoker or operator identity so the compiler
// rejects passing the wrong one.
type CustomerID uuid.UUID

// Invoker is the user / API key / delegated principal that invoked
// usage. It is attribution + budgeting, never ownership.
type Invoker string

// InvokerType classifies whether an invoker is the payer acting directly or a
// delegated principal using the payer's billing authority.
type InvokerType string

const (
	// InvokerTypeDelegated means the invoker is a third-party/member/federated
	// user under the payer; flat per-invoker abuse cutoffs apply.
	InvokerTypeDelegated InvokerType = "delegated"
	// InvokerTypePayer means the invoker is a direct payer-controlled credential;
	// wasted-spend reports use payer grace then charge overage.
	InvokerTypePayer InvokerType = "payer"
)

// String returns the canonical string form of the customer id.
func (id CustomerID) String() string { return uuid.UUID(id).String() }

// UUID returns the underlying uuid.UUID for use in queries.
func (id CustomerID) UUID() uuid.UUID { return uuid.UUID(id) }

// IsZero reports whether the customer id is unset.
func (id CustomerID) IsZero() bool { return uuid.UUID(id) == uuid.Nil }

// String returns the invoker id as a plain string.
func (i Invoker) String() string { return string(i) }

// IsZero reports whether the invoker id is empty.
func (i Invoker) IsZero() bool { return strings.TrimSpace(string(i)) == "" }

// NormalizeInvokerType treats empty/unknown values as delegated. That fails
// closed into the stricter abuse cutoff unless the host explicitly marks the
// request as a direct payer credential.
func NormalizeInvokerType(s string) InvokerType {
	if strings.TrimSpace(s) == string(InvokerTypePayer) {
		return InvokerTypePayer
	}
	return InvokerTypeDelegated
}

// IsDirectPayerInvoker reports whether s is the direct-payer credential type.
func IsDirectPayerInvoker(s string) bool {
	return NormalizeInvokerType(s) == InvokerTypePayer
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
