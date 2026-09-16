package openrails

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DeclaredBilling is one merchant's declared billing facts: what a host knows
// to be true at AsOf, classified by OpenRails through the same pipeline the
// pull/probe/webhook planes use. Imports are idempotent by the rows' stable
// identities; re-posting the same book at the same as_of is a no-op.
type DeclaredBilling struct {
	// AsOf is the evidence horizon: every classification evaluates against it,
	// never wall-clock, so the same book classifies identically whenever it runs.
	AsOf time.Time `json:"as_of"`
	// SubscriptionsExhaustive declares that this call covers the merchant's
	// ENTIRE subscription book, so every local subscription it omits is
	// cancelled. It must be false for batched imports.
	SubscriptionsExhaustive bool `json:"subscriptions_exhaustive,omitempty"`
	// ExpectedSubscriptions is the typed confirmation required alongside
	// SubscriptionsExhaustive; a mismatch refuses the whole import.
	ExpectedSubscriptions *int `json:"expected_subscriptions,omitempty"`
	// DefaultPSP attributes every declared provider row that names no PSP of
	// its own. A row that resolves to neither refuses the whole import.
	DefaultPSP     PSPRef                  `json:"default_psp,omitzero"`
	Customers      []DeclaredCustomer      `json:"customers,omitempty"`
	PaymentMethods []DeclaredPaymentMethod `json:"payment_methods,omitempty"`
	Subscriptions  []DeclaredSubscription  `json:"subscriptions,omitempty"`
	Transactions   []DeclaredTransaction   `json:"transactions,omitempty"`
	// AdminGrants are comped access windows with no payment behind them.
	AdminGrants []DeclaredAdminGrant `json:"admin_grants,omitempty"`
}

// DeclaredCustomer ensures a customer row for a host subject.
type DeclaredCustomer struct {
	Customer uuid.UUID `json:"customer"`
	Email    string    `json:"email,omitempty"`
}

// PSPRef names the PSP a declared row belongs to: the psps row id, or the
// merchant's manifest PSP key. Exactly one form is set.
type PSPRef struct {
	ID  *uuid.UUID `json:"id,omitempty"`
	Key string     `json:"key,omitempty"`
}

// IsZero reports whether the ref names nothing.
func (r PSPRef) IsZero() bool {
	return (r.ID == nil || *r.ID == uuid.Nil) && strings.TrimSpace(r.Key) == ""
}

func (r PSPRef) String() string {
	if r.ID != nil && *r.ID != uuid.Nil {
		return r.ID.String()
	}
	return strings.TrimSpace(r.Key)
}

// DeclaredPaymentMethod is a stored instrument fact, idempotent by
// (psp, rail_customer_ref, rail_method_ref).
type DeclaredPaymentMethod struct {
	Customer uuid.UUID `json:"customer"`
	Rail     string    `json:"rail"`
	// PSP is the account holding the vault entry; falls back to DefaultPSP.
	PSP                  PSPRef    `json:"psp,omitzero"`
	RailCustomerRef      string    `json:"rail_customer_ref"`
	RailMethodRef        string    `json:"rail_method_ref"`
	InitialTransactionID string    `json:"initial_transaction_id,omitempty"`
	LastFour             string    `json:"last_four,omitempty"`
	CardType             string    `json:"card_type,omitempty"`
	ExpiryDate           string    `json:"expiry_date,omitempty"`
	CreatedAt            time.Time `json:"created_at,omitempty"`
}

// PaymentMethodRef links a DeclaredSubscription to a DeclaredPaymentMethod.
type PaymentMethodRef struct {
	Rail            string `json:"rail"`
	RailCustomerRef string `json:"rail_customer_ref"`
	RailMethodRef   string `json:"rail_method_ref"`
}

// CancelEvidence is explicit, settled cancel history. Kind is "" (none),
// "user_cancelled", "chargeback" or "provider_terminated".
type CancelEvidence struct {
	Kind string    `json:"kind,omitempty"`
	At   time.Time `json:"at,omitempty"`
	// ScheduleLive means the provider-side recurring schedule was not confirmed
	// dead at AsOf; rails that delete remotely schedule the deferred delete.
	ScheduleLive bool `json:"schedule_live,omitempty"`
}

// DunningEvidence is the host's dunning state at AsOf.
type DunningEvidence struct {
	Retries      int        `json:"retries"`
	LastRetryAt  *time.Time `json:"last_retry_at,omitempty"`
	NextRetryAt  *time.Time `json:"next_retry_at,omitempty"`
	ScheduleLive bool       `json:"schedule_live"`
}

// DeclaredTransaction is one charge-level fact, successes and declines alike.
// AmountCents is the provider's minor unit.
type DeclaredTransaction struct {
	RailSubscriptionID string    `json:"rail_subscription_id"`
	TransactionID      string    `json:"transaction_id"`
	Type               string    `json:"type,omitempty"` // sale | refund | chargeback | decline; "" = sale
	Success            bool      `json:"success"`
	AmountCents        int64     `json:"amount_cents,string"`
	Currency           string    `json:"currency"`
	OccurredAt         time.Time `json:"occurred_at"`
}

// DeclaredSubscription is one subscription's facts, not classifications.
type DeclaredSubscription struct {
	SourceID           string    `json:"source_id"` // host's stable id: idempotency and audit
	Customer           uuid.UUID `json:"customer"`
	Price              uuid.UUID `json:"price"`
	Rail               string    `json:"rail"`
	RailSubscriptionID string    `json:"rail_subscription_id"`
	// PSP is the merchant account owning this subscription at the provider;
	// falls back to DefaultPSP.
	PSP           PSPRef            `json:"psp,omitzero"`
	UserEmail     string            `json:"user_email,omitempty"`
	StartedAt     time.Time         `json:"started_at"`
	PaidThrough   *time.Time        `json:"paid_through,omitempty"`
	Cancel        CancelEvidence    `json:"cancel,omitempty"`
	Dunning       *DunningEvidence  `json:"dunning,omitempty"`
	PaymentMethod *PaymentMethodRef `json:"payment_method,omitempty"`
	// Evidence is the host's verbatim legacy payload, stored for forensics.
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

// DeclaredAdminGrant is a comped product window with no payment behind it,
// idempotent by SourceID. A nil EndsAt is indefinite.
type DeclaredAdminGrant struct {
	Customer uuid.UUID  `json:"customer"`
	Product  uuid.UUID  `json:"product"`
	SourceID string     `json:"source_id"`
	StartsAt time.Time  `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`
}

// BillingImportResult reports per-SourceID outcomes for subscriptions and
// admin grants; customers and payment methods are idempotent upserts.
type BillingImportResult struct {
	Imported []string          `json:"imported"`
	Skipped  []string          `json:"skipped"`
	Blocked  []string          `json:"blocked"`
	Reasons  map[string]string `json:"reasons"`
}

// ImportBilling lands declared billing facts under the bound merchant.
func (c *Client) ImportBilling(ctx context.Context, book DeclaredBilling) (*BillingImportResult, error) {
	if book.AsOf.IsZero() {
		return nil, invalidErr("as_of is required")
	}
	var out BillingImportResult
	if err := c.do(ctx, http.MethodPost, "/v1/import/billing", book, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
