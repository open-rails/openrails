package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
)

// RefundPayment refunds a charge through its rail. The returned refund has
// Status "succeeded" once the provider confirmed it, or "pending" while the
// durable refund operation is parked (provider write gates) or reconciling an
// uncertain provider outcome; it settles without another call. A provider
// refusal is a StatusError and leaves no refund recorded.
func (c *Client) RefundPayment(ctx context.Context, id billing.PaymentID, params billing.RefundPaymentParams, requestOptions ...RequestOption) (*billing.Payment, error) {
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
	var out billing.Payment
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/payments/"+payment+"/refunds", params, &out, http.Header{"Idempotency-Key": {params.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

type archiveProductRequest struct {
	ProductID  string                  `json:"product_id,omitempty"`
	ProductKey string                  `json:"product_key,omitempty"`
	Purchases  archiveProductPurchases `json:"purchases"`
	Reason     string                  `json:"reason,omitempty"`
}

type archiveProductPurchases struct {
	Action         billing.PurchaseAction `json:"action"`
	PurchasedSince *time.Time             `json:"purchased_since,omitempty"`
	Window         string                 `json:"window,omitempty"`
}

// ArchiveProduct archives a product (never deletes it) and, per the caller's
// policy, refunds or queues for review its recent one-time purchases.
func (c *Client) ArchiveProduct(ctx context.Context, params billing.ArchiveProductParams, requestOptions ...RequestOption) (*billing.ProductArchive, error) {
	if strings.TrimSpace(params.IdempotencyKey) == "" {
		return nil, invalidErr("Idempotency-Key required")
	}
	request := archiveProductRequest{ProductID: params.ProductID, ProductKey: params.ProductKey, Reason: params.Reason, Purchases: archiveProductPurchases{Action: params.Action}}
	if request.Purchases.Action == "" {
		request.Purchases.Action = billing.PurchaseActionNone
	}
	if !params.PurchasedSince.IsZero() {
		since := params.PurchasedSince.UTC()
		request.Purchases.PurchasedSince = &since
	}
	if params.Window != 0 {
		request.Purchases.Window = params.Window.String()
	}
	var out billing.ProductArchive
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/catalog/product-archives", request, &out, http.Header{"Idempotency-Key": {params.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetProductArchive reads an archive operation with the current outcome of
// each qualifying purchase. It performs no refunds.
func (c *Client) GetProductArchive(ctx context.Context, id string, requestOptions ...RequestOption) (*billing.ProductArchive, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalidErr("product archive id required")
	}
	var out billing.ProductArchive
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/product-archives/"+url.PathEscape(id), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPurchaseReviews is one page of purchase reviews, oldest first.
func (c *Client) ListPurchaseReviews(ctx context.Context, filter billing.ListPurchaseReviewsParams, requestOptions ...RequestOption) (*billing.ListPage[billing.PurchaseReview], error) {
	q := pageValues(nil, filter.Page)
	if status := strings.TrimSpace(filter.Status); status != "" {
		q.Set("status", status)
	}
	if archive := strings.TrimSpace(filter.ProductArchiveID); archive != "" {
		q.Set("product_archive_id", archive)
	}
	var out billing.ListPage[billing.PurchaseReview]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/purchase-reviews?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResolvePurchaseReview refunds or dismisses one review.
func (c *Client) ResolvePurchaseReview(ctx context.Context, id string, params billing.ResolvePurchaseReviewParams, requestOptions ...RequestOption) (*billing.PurchaseReview, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalidErr("purchase review id required")
	}
	switch params.Decision {
	case billing.PurchaseReviewDecisionRefund, billing.PurchaseReviewDecisionDismiss:
	default:
		return nil, invalidErr("decision must be " + strconv.Quote(string(billing.PurchaseReviewDecisionRefund)) + " or " + strconv.Quote(string(billing.PurchaseReviewDecisionDismiss)))
	}
	var out billing.PurchaseReview
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/purchase-reviews/"+url.PathEscape(id)+"/resolve", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
