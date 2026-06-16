package models

import (
	"time"

	"github.com/google/uuid"
)

// PaymentBlocklistEntry is one merchant-scoped block of a known-bad payment
// identifier (issue #300). Checkout/admission can later DENY a payment whose
// (kind, value) matches an entry.
//
// An entry is either merchant-wide (CustomerID nil/zero) — applying to every payer
// org in the merchant — or scoped to a single merchant subject (CustomerID set, issue #221).
// Uniqueness is (merchant_id, kind, value); that index is also the IsBlocked lookup.
type PaymentBlocklistEntry struct {
	ID uuid.UUID `json:"id"`
	// MerchantID scopes this row to a merchant / billing namespace (issue #223/#227).
	MerchantID uuid.UUID `json:"merchant_id"`
	// CustomerID, when set, scopes the block to a single merchant subject (issue #221).
	// Nil/zero means a merchant-wide block.
	CustomerID *uuid.UUID `json:"customer_id,omitempty"`
	// Kind is the kind of identifier blocked:
	// card_fingerprint | processor_customer | email | ip.
	Kind string `json:"kind"`
	// Value is the blocked identifier value.
	Value string `json:"value"`
	// Reason is an optional free-form note for the block (audit).
	Reason    *string   `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
