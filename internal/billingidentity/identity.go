// Package identity provides explicit, mutually-distinct identity types for
// OpenRails openrails.
package billingidentity

import (
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
)

// CustomerID identifies a billing.customers row: the OpenRails payable
// subject whose balance, invoices, reservations, and entitlements are
// recorded. It is the shared wire type billing.CustomerID (one family, one
// spelling), distinct from any invoker or operator identity so the compiler
// rejects passing the wrong one.
type CustomerID = billing.CustomerID

// InvokerType is the wire vocabulary billing.InvokerType.
type InvokerType = billing.InvokerType

const (
	InvokerTypeDelegated = billing.InvokerTypeDelegated
	InvokerTypeCustomer  = billing.InvokerTypeCustomer
)

// IsDirectPayerInvoker reports whether t is the direct-payer credential type.
// Anything else is delegated: the stricter abuse cutoff.
func IsDirectPayerInvoker[T ~string](t T) bool {
	return strings.TrimSpace(string(t)) == string(InvokerTypeCustomer)
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
