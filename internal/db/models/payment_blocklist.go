package models

import (
	"time"

	"github.com/google/uuid"
)

// PaymentBlocklistEntry is one tenant-scoped block of a known-bad payment
// identifier (issue #300). Checkout/admission can later DENY a payment whose
// (kind, value) matches an entry.
//
// An entry is either tenant-wide (MerchantSubjectID nil/zero) — applying to every payer
// org in the tenant — or scoped to a single tenant subject (MerchantSubjectID set, issue #221).
// Uniqueness is (tenant_id, kind, value); that index is also the IsBlocked lookup.
type PaymentBlocklistEntry struct {
	ID uuid.UUID `json:"id"`
	// MerchantID scopes this row to a tenant / billing namespace (issue #223/#227).
	MerchantID uuid.UUID `json:"tenant_id"`
	// MerchantSubjectID, when set, scopes the block to a single tenant subject (issue #221).
	// Nil/zero means a tenant-wide block.
	MerchantSubjectID *uuid.UUID `json:"tenant_subject_id,omitempty"`
	// Kind is the kind of identifier blocked:
	// card_fingerprint | processor_customer | email | ip.
	Kind string `json:"kind"`
	// Value is the blocked identifier value.
	Value string `json:"value"`
	// Reason is an optional free-form note for the block (audit).
	Reason    *string   `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
