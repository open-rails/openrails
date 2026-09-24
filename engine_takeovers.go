package openrails

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Engine takeover moves a legacy NMI-billed subscription to OpenRails billing
// at its next period boundary: the NMI schedule is deleted at least a day
// before the boundary, the legacy subscription ends with its paid access, and
// an engine-owned successor charges the same vaulted card from the boundary.
const (
	CodeEngineTakeoverIneligible       = "engine_takeover_ineligible"
	CodeEngineTakeoverNoAgreement      = "engine_takeover_no_recurring_agreement"
	CodeEngineTakeoverBoundaryTooClose = "engine_takeover_boundary_too_close"
	CodeEngineTakeoverInFlight         = "engine_takeover_in_flight"
	CodeEngineTakeoverCommitted        = "engine_takeover_committed"
	CodeEngineTakeoverConflict         = "engine_takeover_conflict"
	CodeEngineTakeoverNotFound         = "engine_takeover_not_found"
	CodeEngineTakeoverRateLimited      = "engine_takeover_rate_limited"
)

// EngineTakeover is one takeover operation. Stage is ready (preview),
// pending, held (parked by the destructive switch or breaker),
// delete_submitted, completed, not_executed or abandoned.
type EngineTakeover struct {
	ID                      uuid.UUID       `json:"id,omitzero"`
	SubscriptionID          SubscriptionID  `json:"subscription_id"`
	SuccessorSubscriptionID *SubscriptionID `json:"successor_subscription_id,omitempty"`
	RailSubscriptionID      string          `json:"rail_subscription_id"`
	Anchor                  time.Time       `json:"anchor"`
	Cutoff                  time.Time       `json:"cutoff"`
	Amount                  int64           `json:"amount,string"`
	Currency                string          `json:"currency"`
	Status                  string          `json:"status"`
	Stage                   string          `json:"stage"`
	Reason                  string          `json:"reason,omitempty"`
}

// EngineTakeoverBatchRequest admits takeovers for up to MaxSubscriptions
// eligible legacy NMI subscriptions (earliest boundary first), optionally of
// one price.
type EngineTakeoverBatchRequest struct {
	MaxSubscriptions int    `json:"max_subscriptions"`
	PriceID          string `json:"price_id,omitempty"`
}

type EngineTakeoverRefusal struct {
	SubscriptionID SubscriptionID `json:"subscription_id"`
	Code           string         `json:"code"`
	Reason         string         `json:"reason"`
}

type EngineTakeoverBatchResult struct {
	Admitted []EngineTakeover        `json:"admitted"`
	Refused  []EngineTakeoverRefusal `json:"refused"`
}

func (c *Client) PreviewEngineTakeover(ctx context.Context, subscriptionID SubscriptionID, requestOptions ...RequestOption) (*EngineTakeover, error) {
	return c.engineTakeover(ctx, http.MethodPost, subscriptionID, "/engine-takeover/preview", nil, requestOptions...)
}

// TakeOverBilling admits and runs the takeover; the same key replays it.
func (c *Client) TakeOverBilling(ctx context.Context, subscriptionID SubscriptionID, idempotencyKey string, requestOptions ...RequestOption) (*EngineTakeover, error) {
	if idempotencyKey == "" {
		return nil, invalidErr("idempotency key required")
	}
	return c.engineTakeover(ctx, http.MethodPost, subscriptionID, "/engine-takeover", http.Header{"Idempotency-Key": {idempotencyKey}}, requestOptions...)
}

// GetEngineTakeover reads the subscription's latest takeover.
func (c *Client) GetEngineTakeover(ctx context.Context, subscriptionID SubscriptionID, requestOptions ...RequestOption) (*EngineTakeover, error) {
	return c.engineTakeover(ctx, http.MethodGet, subscriptionID, "/engine-takeover", nil, requestOptions...)
}

// AbandonEngineTakeover ends a takeover that has not yet changed NMI.
func (c *Client) AbandonEngineTakeover(ctx context.Context, subscriptionID SubscriptionID, requestOptions ...RequestOption) (*EngineTakeover, error) {
	return c.engineTakeover(ctx, http.MethodPost, subscriptionID, "/engine-takeover/abandon", nil, requestOptions...)
}

// TakeOverBillingBatch admits takeovers in bulk; the executor runs them under
// the destructive switch and volume breaker.
func (c *Client) TakeOverBillingBatch(ctx context.Context, request EngineTakeoverBatchRequest, requestOptions ...RequestOption) (*EngineTakeoverBatchResult, error) {
	if request.MaxSubscriptions <= 0 {
		return nil, invalidErr("max_subscriptions must be positive")
	}
	var out EngineTakeoverBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/engine-takeovers", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) engineTakeover(ctx context.Context, method string, subscriptionID SubscriptionID, suffix string, headers http.Header, requestOptions ...RequestOption) (*EngineTakeover, error) {
	path, err := subscriptionPath(subscriptionID)
	if err != nil {
		return nil, err
	}
	var body any
	if method == http.MethodPost {
		body = struct{}{}
	}
	var out EngineTakeover
	if err := c.doWithHeaders(ctx, method, path+suffix, body, &out, headers, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
