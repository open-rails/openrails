package billing

import (
	"time"
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

// PurchaseAction is what a product archive does with purchases inside its window.
type PurchaseAction string

const (
	// PurchaseActionNone archives the product only.
	PurchaseActionNone PurchaseAction = "none"
	// PurchaseActionRefund refunds each qualifying purchase in full and ends the
	// access that purchase granted. Purchases OpenRails cannot refund
	// automatically become purchase reviews instead.
	PurchaseActionRefund PurchaseAction = "refund"
	// PurchaseActionReview records each qualifying purchase as a purchase review
	// for the merchant to refund or dismiss.
	PurchaseActionReview PurchaseAction = "review"
)

// ArchiveProductParams archives one product (ProductID or ProductKey) and
// applies Action to its one-time purchases made at or after PurchasedSince, or
// within Window before the operation was first accepted. The window is fixed
// at first acceptance; retries with the same IdempotencyKey evaluate the same
// purchases and never refund twice.
type ArchiveProductParams struct {
	ProductID      string
	ProductKey     string
	Action         PurchaseAction
	PurchasedSince time.Time
	Window         time.Duration
	Reason         string
	IdempotencyKey string
}

// ProductArchive is the durable result of ArchiveProduct.
type ProductArchive struct {
	ID             string         `json:"id"`
	Object         string         `json:"object"`
	ProductID      ProductID      `json:"product_id"`
	ProductKey     string         `json:"product_key"`
	Action         PurchaseAction `json:"purchase_action"`
	PurchasedSince *time.Time     `json:"purchased_since,omitempty"`
	Reason         string         `json:"reason,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	// Complete is false while qualifying purchases remain unprocessed; retry
	// the same operation to continue.
	Complete  bool               `json:"complete"`
	Purchases []ArchivedPurchase `json:"purchases"`
}

// Archived purchase outcomes.
const (
	ArchivedPurchaseRefunded        = "refunded"
	ArchivedPurchaseRefundPending   = "refund_pending"
	ArchivedPurchaseAlreadyRefunded = "already_refunded"
	ArchivedPurchaseReviewOpen      = "review_open"
	ArchivedPurchaseReviewRefunded  = "review_refunded"
	ArchivedPurchaseReviewDismissed = "review_dismissed"
	ArchivedPurchaseNotStarted      = "not_started"
)

// ArchivedPurchase is one qualifying purchase and what the archive did with it.
type ArchivedPurchase struct {
	PaymentID   PaymentID  `json:"payment_id"`
	CustomerID  string     `json:"customer_id"`
	Amount      int64      `json:"amount,string"`
	Currency    string     `json:"currency"`
	PurchasedAt time.Time  `json:"purchased_at"`
	Outcome     string     `json:"outcome"`
	RefundID    *PaymentID `json:"refund_id,omitempty"`
	ReviewID    string     `json:"review_id,omitempty"`
	Detail      string     `json:"detail,omitempty"`
}

// Purchase review statuses.
const (
	PurchaseReviewOpen      = "open"
	PurchaseReviewRefunded  = "refunded"
	PurchaseReviewDismissed = "dismissed"
)

// PurchaseReview is a purchase an archive recorded for merchant review.
type PurchaseReview struct {
	ID               string     `json:"id"`
	Status           string     `json:"status"`
	ProductArchiveID string     `json:"product_archive_id"`
	ProductID        ProductID  `json:"product_id"`
	ProductKey       string     `json:"product_key"`
	PaymentID        PaymentID  `json:"payment_id"`
	CustomerID       string     `json:"customer_id"`
	Amount           int64      `json:"amount,string"`
	Currency         string     `json:"currency"`
	PurchasedAt      time.Time  `json:"purchased_at"`
	Detail           string     `json:"detail,omitempty"`
	RefundID         *PaymentID `json:"refund_id,omitempty"`
	Notes            string     `json:"notes,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
}

// ListPurchaseReviewsParams selects reviews, oldest first; Status defaults to
// open.
type ListPurchaseReviewsParams struct {
	Page             PageRequest
	Status           string
	ProductArchiveID string
}

// PurchaseReviewDecision resolves a review.
type PurchaseReviewDecision string

const (
	// PurchaseReviewDecisionRefund refunds the remaining amount of the purchase
	// and ends the access it granted.
	PurchaseReviewDecisionRefund PurchaseReviewDecision = "refund"
	// PurchaseReviewDecisionDismiss keeps the purchase and its access.
	PurchaseReviewDecisionDismiss PurchaseReviewDecision = "dismiss"
)

// ResolvePurchaseReviewParams is one merchant decision. Resolving a review
// again with the same decision returns it unchanged.
type ResolvePurchaseReviewParams struct {
	Decision PurchaseReviewDecision `json:"decision"`
	Notes    string                 `json:"notes,omitempty"`
}
