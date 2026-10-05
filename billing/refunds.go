package billing

import (
	"time"

	"github.com/google/uuid"
)

var (
	// ErrRefundRailUnavailable: the payment's rail cannot currently accept a
	// refund (account not armed or not configured). Nothing was reserved.
	ErrRefundRailUnavailable error = newCodedError("refund_rail_unavailable", ErrConflict)
	// ErrRefundUnsupported: the payment's rail has no automatic refund (CCBill,
	// off-rail channels). Refund it at the provider and record it there.
	ErrRefundUnsupported error = newCodedError("refund_unsupported", ErrInvalid)
)

// RefundPaymentParams refunds one charge. Exactly one of Amount (native units
// at the payment currency's scale) or Full is required. IdempotencyKey is
// mandatory: a retry with the same key returns the same refund and never
// refunds twice; the same key with different terms is ErrIdempotencyKeyReused.
// RevokeAccess also ends the entitlements and product access that the payment
// granted, in the same transaction that records the refund. For a membership
// payment it ends the membership's current access, and an engine-owned
// membership is canceled so it does not renew; without it the membership
// continues. The provider's notification of an OpenRails refund never
// changes this decision.
type RefundPaymentParams struct {
	Amount         int64  `json:"amount,omitempty,string"`
	Full           bool   `json:"full,omitempty"`
	Reason         string `json:"reason,omitempty"`
	RevokeAccess   bool   `json:"revoke_access,omitempty"`
	IdempotencyKey string `json:"-"`
}

// ProductArchiveID names one product archive operation; on the wire
// "par_<uuid>".
type ProductArchiveID uuid.UUID

const productArchiveIDPrefix = "par_"

func ParseProductArchiveID(s string) (ProductArchiveID, error) {
	u, err := parsePrefixedID("product archive", productArchiveIDPrefix, s)
	return ProductArchiveID(u), err
}

func (id ProductArchiveID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id ProductArchiveID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id ProductArchiveID) String() string {
	return formatPrefixedID(productArchiveIDPrefix, uuid.UUID(id))
}
func (id ProductArchiveID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *ProductArchiveID) UnmarshalText(b []byte) error {
	v, err := ParseProductArchiveID(string(b))
	*id = v
	return err
}

// PurchaseAction is what a product archive does with purchases inside its window.
type PurchaseAction string

const (
	// PurchaseActionNone archives the product only.
	PurchaseActionNone PurchaseAction = "none"
	// PurchaseActionRefund refunds each qualifying purchase in full and ends the
	// access that purchase granted. A purchase OpenRails cannot refund
	// automatically becomes a finding for the merchant to resolve instead.
	PurchaseActionRefund PurchaseAction = "refund"
	// PurchaseActionReview records each qualifying purchase as a finding for
	// the merchant to refund (approve) or keep (ignore).
	PurchaseActionReview PurchaseAction = "review"
)

// ArchiveProductParams archives one product (ProductID or ProductKey) and
// applies PurchaseAction (empty: none) to its one-time purchases made at or
// after PurchasedSince, or within WindowSeconds before the operation was first
// accepted. The window is fixed at first acceptance; retries with the same
// IdempotencyKey evaluate the same purchases and never refund twice.
type ArchiveProductParams struct {
	ProductID      ProductID      `json:"product_id,omitzero"`
	ProductKey     string         `json:"product_key,omitempty"`
	PurchaseAction PurchaseAction `json:"purchase_action,omitempty"`
	PurchasedSince time.Time      `json:"purchased_since,omitzero"`
	WindowSeconds  int64          `json:"window_seconds,omitempty"`
	Reason         string         `json:"reason,omitempty"`
	IdempotencyKey string         `json:"-"`
}

// ProductArchive is the durable result of ArchiveProduct.
type ProductArchive struct {
	ID             ProductArchiveID `json:"id"`
	ProductID      ProductID        `json:"product_id"`
	ProductKey     string           `json:"product_key"`
	PurchaseAction PurchaseAction   `json:"purchase_action"`
	PurchasedSince *time.Time       `json:"purchased_since"`
	Reason         *string          `json:"reason"`
	CreatedAt      time.Time        `json:"created_at"`
	// Complete is false while qualifying purchases remain unprocessed; retry
	// the same operation to continue.
	Complete  bool               `json:"complete"`
	Purchases []ArchivedPurchase `json:"purchases"`
}

// ArchivedPurchaseOutcome is what an archive did with one purchase.
type ArchivedPurchaseOutcome string

const (
	ArchivedPurchaseRefunded        ArchivedPurchaseOutcome = "refunded"
	ArchivedPurchaseRefundPending   ArchivedPurchaseOutcome = "refund_pending"
	ArchivedPurchaseAlreadyRefunded ArchivedPurchaseOutcome = "already_refunded"
	ArchivedPurchaseReviewOpen      ArchivedPurchaseOutcome = "review_open"
	ArchivedPurchaseReviewRefunded  ArchivedPurchaseOutcome = "review_refunded"
	ArchivedPurchaseReviewDismissed ArchivedPurchaseOutcome = "review_dismissed"
	ArchivedPurchaseNotStarted      ArchivedPurchaseOutcome = "not_started"
)

// ArchivedPurchase is one qualifying purchase and what the archive did with
// it. FindingID is the finding a purchase under review is resolved through
// (ResolveFinding: approve refunds it, ignore keeps it).
type ArchivedPurchase struct {
	PaymentID   PaymentID               `json:"payment_id"`
	CustomerID  CustomerID              `json:"customer_id"`
	Amount      int64                   `json:"amount,string"`
	Currency    string                  `json:"currency"`
	PurchasedAt time.Time               `json:"purchased_at"`
	Outcome     ArchivedPurchaseOutcome `json:"outcome"`
	RefundID    *PaymentID              `json:"refund_id"`
	FindingID   *FindingID              `json:"finding_id"`
	Detail      *string                 `json:"detail"`
}
