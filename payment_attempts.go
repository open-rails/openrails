package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
)

// ListPaymentAttempts lists the merchant's payment attempts, newest first.
func (c *Client) ListPaymentAttempts(ctx context.Context, filter billing.PaymentAttemptFilter, requestOptions ...RequestOption) (*billing.Page[billing.PaymentAttempt], error) {
	q := pageQuery(filter.PageOptions)
	setQuery(q, map[string]string{"kind": commaList(filter.Kind), "owner": commaList(filter.Owner), "category": commaList(filter.Category), "reason": commaList(filter.Reason),
		"response_code": commaList(filter.ResponseCode), "card_entry": commaList(filter.CardEntry), "source": commaList(filter.Source),
		"observed_via": commaList(filter.ObservedVia), "avs_result": commaList(filter.AVSResult), "cvv_result": commaList(filter.CVVResult),
		"psp_id": filter.PSPID, "customer_id": filter.CustomerID,
		"checkout_id": filter.CheckoutID, "subscription_id": filter.SubscriptionID.String(), "cycle_id": filter.CycleID.String(),
		"since": timeQuery(filter.Since), "until": timeQuery(filter.Until)})
	var out billing.Page[billing.PaymentAttempt]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/payment-attempts?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPaymentAttempt reads one payment attempt.
func (c *Client) GetPaymentAttempt(ctx context.Context, id billing.PaymentAttemptID, requestOptions ...RequestOption) (*billing.PaymentAttempt, error) {
	attempt, err := requireTypedID("payment_attempt_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.PaymentAttempt
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/payment-attempts/"+attempt, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRebillCycles lists the merchant's rebill cycles, latest due first.
func (c *Client) ListRebillCycles(ctx context.Context, filter billing.RebillCycleFilter, requestOptions ...RequestOption) (*billing.Page[billing.RebillCycle], error) {
	q := pageQuery(filter.PageOptions)
	setQuery(q, map[string]string{"owner": commaList(filter.Owner), "first_outcome": commaList(filter.FirstOutcome), "miss_reason": commaList(filter.MissReason),
		"outcome": commaList(filter.Outcome), "psp_id": filter.PSPID, "subscription_id": filter.SubscriptionID.String(),
		"due_since": timeQuery(filter.DueSince), "due_until": timeQuery(filter.DueUntil)})
	var out billing.Page[billing.RebillCycle]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/rebill-cycles?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRebillCycle reads one rebill cycle with its attempts.
func (c *Client) GetRebillCycle(ctx context.Context, id billing.RebillCycleID, requestOptions ...RequestOption) (*billing.RebillCycle, error) {
	cycle, err := requireTypedID("rebill_cycle_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.RebillCycle
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/rebill-cycles/"+cycle, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func setQuery(q url.Values, values map[string]string) {
	for key, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			q.Set(key, value)
		}
	}
}

func commaList(values []string) string { return strings.Join(values, ",") }

func timeQuery(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
