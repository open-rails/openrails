// Package identity provides explicit, mutually-distinct identity types for
// OpenRails openrails.
package billingidentity

import (
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
)

// CustomerID identifies an openrails.customers row: the OpenRails payable
// subject whose balance, invoices, reservations, and entitlements are
// recorded. It is the shared wire type openrails.CustomerID (one family, one
// spelling), distinct from any invoker or operator identity so the compiler
// rejects passing the wrong one.
type CustomerID = openrails.CustomerID

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
