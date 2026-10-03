package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
// membership is cancelled so it does not renew; without it the membership
// continues. The provider's notification of an OpenRails refund never
// changes this decision.
type RefundPaymentParams struct {
	Amount         int64  `json:"amount,omitempty,string"`
	Full           bool   `json:"full,omitempty"`
	Reason         string `json:"reason,omitempty"`
	RevokeAccess   bool   `json:"revoke_access,omitempty"`
	IdempotencyKey string `json:"-"`
}

// RefundPayment refunds a charge through its rail. The returned refund object
// has Status "succeeded" once the provider confirmed it, or "pending" while the
// durable refund operation is parked (provider write gates) or reconciling an
// uncertain provider outcome; it settles without another call. A provider
// refusal is a StatusError and leaves no refund recorded.
func (c *Client) RefundPayment(ctx context.Context, id PaymentID, params RefundPaymentParams, requestOptions ...RequestOption) (*Payment, error) {
	payment, err := requireTypedID("payment_id", id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.IdempotencyKey) == "" {
		return nil, invalidErr("Idempotency-Key required")
	}
	if params.Full == (params.Amount != 0) || params.Amount < 0 {
		return nil, invalidErr("exactly one of a positive amount or full is required")
	}
	var out Payment
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/payments/"+payment+"/refunds", params, &out, http.Header{"Idempotency-Key": {params.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
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

type archiveProductRequest struct {
	ProductID  string                  `json:"product_id,omitempty"`
	ProductKey string                  `json:"product_key,omitempty"`
	Purchases  archiveProductPurchases `json:"purchases"`
	Reason     string                  `json:"reason,omitempty"`
}

type archiveProductPurchases struct {
	Action         PurchaseAction `json:"action"`
	PurchasedSince *time.Time     `json:"purchased_since,omitempty"`
	Window         string         `json:"window,omitempty"`
}

// ArchiveProduct archives a product (never deletes it) and, per the caller's
// policy, refunds or queues for review its recent one-time purchases.
func (c *Client) ArchiveProduct(ctx context.Context, params ArchiveProductParams, requestOptions ...RequestOption) (*ProductArchive, error) {
	if strings.TrimSpace(params.IdempotencyKey) == "" {
		return nil, invalidErr("Idempotency-Key required")
	}
	request := archiveProductRequest{ProductID: params.ProductID, ProductKey: params.ProductKey, Reason: params.Reason, Purchases: archiveProductPurchases{Action: params.Action}}
	if request.Purchases.Action == "" {
		request.Purchases.Action = PurchaseActionNone
	}
	if !params.PurchasedSince.IsZero() {
		since := params.PurchasedSince.UTC()
		request.Purchases.PurchasedSince = &since
	}
	if params.Window != 0 {
		request.Purchases.Window = params.Window.String()
	}
	var out ProductArchive
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/catalog/product-archives", request, &out, http.Header{"Idempotency-Key": {params.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetProductArchive reads an archive operation with the current outcome of
// each qualifying purchase. It performs no refunds.
func (c *Client) GetProductArchive(ctx context.Context, id string, requestOptions ...RequestOption) (*ProductArchive, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalidErr("product archive id required")
	}
	var out ProductArchive
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/product-archives/"+url.PathEscape(id), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
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
	Object           string     `json:"object"`
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

// PurchaseReviewFilter selects reviews; Status defaults to open.
type PurchaseReviewFilter struct {
	PageOptions
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

// ListPurchaseReviews lists purchase reviews, oldest first.
func (c *Client) ListPurchaseReviews(ctx context.Context, filter PurchaseReviewFilter, requestOptions ...RequestOption) (*Page[PurchaseReview], error) {
	q := pageQuery(filter.PageOptions)
	if status := strings.TrimSpace(filter.Status); status != "" {
		q.Set("status", status)
	}
	if archive := strings.TrimSpace(filter.ProductArchiveID); archive != "" {
		q.Set("product_archive_id", archive)
	}
	var out Page[PurchaseReview]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/purchase-reviews?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResolvePurchaseReview refunds or dismisses one review.
func (c *Client) ResolvePurchaseReview(ctx context.Context, id string, params ResolvePurchaseReviewParams, requestOptions ...RequestOption) (*PurchaseReview, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalidErr("purchase review id required")
	}
	switch params.Decision {
	case PurchaseReviewDecisionRefund, PurchaseReviewDecisionDismiss:
	default:
		return nil, invalidErr("decision must be " + strconv.Quote(string(PurchaseReviewDecisionRefund)) + " or " + strconv.Quote(string(PurchaseReviewDecisionDismiss)))
	}
	var out PurchaseReview
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/purchase-reviews/"+url.PathEscape(id)+"/resolve", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
