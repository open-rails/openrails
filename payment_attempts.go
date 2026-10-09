package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
)

// ListPaymentAttempts is one page of the merchant's payment attempts, newest
// first.
func (c *Client) ListPaymentAttempts(ctx context.Context, filter billing.PaymentAttemptListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.PaymentAttempt], error) {
	q := pageValues(nil, filter.PageRequest)
	setQuery(q, map[string]string{"kind": commaList(filter.Kind), "owner": commaList(filter.Owner), "category": commaList(filter.Category), "reason": commaList(filter.Reason),
		"response_code": commaList(filter.ResponseCode), "card_entry": commaList(filter.CardEntry), "source": commaList(filter.Source),
		"observed_via": commaList(filter.ObservedVia), "avs_result": commaList(filter.AVSResult), "cvv_result": commaList(filter.CVVResult),
		"psp_id": filter.PSPID.String(), "customer_id": filter.CustomerID.String(),
		"checkout_id": filter.CheckoutID, "subscription_id": filter.SubscriptionID.String(), "cycle_id": filter.CycleID.String(),
		"since": timeQuery(filter.Since), "until": timeQuery(filter.Until)})
	if err := setIDs(q, filter.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.PaymentAttempt]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/payment-attempts?"+q.Encode(), nil, &out, requestOptions...); err != nil {
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
	if err := c.do(ctx, http.MethodGet, "/v1/admin/payment-attempts/"+attempt, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRebillCycles is one page of the merchant's rebill cycles, latest due
// first.
func (c *Client) ListRebillCycles(ctx context.Context, filter billing.RebillCycleListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.RebillCycle], error) {
	q := pageValues(nil, filter.PageRequest)
	setQuery(q, map[string]string{"owner": commaList(filter.Owner), "first_outcome": commaList(filter.FirstOutcome), "miss_reason": commaList(filter.MissReason),
		"outcome": commaList(filter.Outcome), "psp_id": filter.PSPID.String(), "subscription_id": filter.SubscriptionID.String(),
		"due_since": timeQuery(filter.DueSince), "due_until": timeQuery(filter.DueUntil)})
	if err := setIDs(q, filter.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.RebillCycle]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/rebill-cycles?"+q.Encode(), nil, &out, requestOptions...); err != nil {
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
	if err := c.do(ctx, http.MethodGet, "/v1/admin/rebill-cycles/"+cycle, nil, &out, requestOptions...); err != nil {
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

func commaList[T ~string](values []T) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = string(v)
	}
	return strings.Join(parts, ",")
}

func timeQuery(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
