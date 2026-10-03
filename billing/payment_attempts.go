package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PaymentAttempt is one authorization a PSP answered (#1110): a card
// verification, a sale, a rebill or a retry, approved or not. Category,
// Reason and Action are the one decline classifier's; the codes and texts
// are the PSP's verbatim. Amount is native units ("0" for a verification).
type PaymentAttempt struct {
	ID     PaymentAttemptID `json:"id"`
	Object string           `json:"object"`
	// Kind is verify, initial, upgrade, rebill, dunning_retry, customer_retry
	// or invoice.
	Kind string `json:"kind"`
	// Owner is who collects the subscription: engine, nmi_schedule, provider
	// or none.
	Owner string `json:"owner"`
	// CardEntry is new (entered in this checkout or card add) or saved.
	CardEntry string `json:"card_entry"`
	// Source is openrails, provider_schedule or external; ObservedVia is how
	// OpenRails learned the answer: response, webhook or pull.
	Source       string        `json:"source"`
	ObservedVia  string        `json:"observed_via"`
	Category     string        `json:"category"`
	Reason       DeclineReason `json:"reason,omitempty"`
	Action       string        `json:"action,omitempty"`
	ResponseCode string        `json:"response_code,omitempty"`
	ResponseText string        `json:"response_text,omitempty"`
	// IssuerCode and IssuerText are the issuer's raw answer, once enriched.
	IssuerCode    string `json:"issuer_code,omitempty"`
	IssuerText    string `json:"issuer_text,omitempty"`
	AVSResult     string `json:"avs_result,omitempty"`
	CVVResult     string `json:"cvv_result,omitempty"`
	CardBrand     string `json:"card_brand,omitempty"`
	CardLast4     string `json:"card_last4,omitempty"`
	CardBIN       string `json:"card_bin,omitempty"`
	TokenType     string `json:"token_type,omitempty"`
	TransactionID string `json:"transaction_id,omitempty"`
	Rail          string `json:"rail"`
	// PSPID is the provider account's plain UUID.
	PSPID       string    `json:"psp_id"`
	CustomerID  string    `json:"customer_id"`
	Amount      int64     `json:"amount,string"`
	Currency    string    `json:"currency,omitempty"`
	AttemptedAt time.Time `json:"attempted_at"`
	// CheckoutID groups one buyer's attempts on one target (a price or
	// card_save, CheckoutTarget) until it is approved.
	CheckoutID      string           `json:"checkout_id,omitempty"`
	CheckoutTarget  string           `json:"checkout_target,omitempty"`
	CycleID         *RebillCycleID   `json:"cycle_id,omitempty"`
	SubscriptionID  *SubscriptionID  `json:"subscription_id,omitempty"`
	PaymentMethodID *PaymentMethodID `json:"payment_method_id,omitempty"`
	PaymentID       *PaymentID       `json:"payment_id,omitempty"`
	EnrichedAt      *time.Time       `json:"enriched_at,omitempty"`
}

// PaymentAttemptFilter selects attempts, newest first; every field is
// optional, and a list matches any of its values. Since and Until bound
// attempted_at to [Since, Until).
type PaymentAttemptFilter struct {
	PageOptions
	Kind, Owner, Category, Reason, ResponseCode, CardEntry []string
	Source, ObservedVia, AVSResult, CVVResult              []string
	// PSPID, CustomerID and CheckoutID are plain UUIDs.
	PSPID, CustomerID, CheckoutID string
	SubscriptionID                SubscriptionID
	CycleID                       RebillCycleID
	Since, Until                  time.Time
}

// RebillCycle is one paid period that came due (#1111) and what its attempts
// decided. Outcome is collected, lost (closed without a collection) or open;
// a cycle closes when collected, when its subscription is cancelled, or 15
// days past due.
type RebillCycle struct {
	ID             RebillCycleID  `json:"id"`
	Object         string         `json:"object"`
	SubscriptionID SubscriptionID `json:"subscription_id"`
	CustomerID     string         `json:"customer_id"`
	PSPID          string         `json:"psp_id"`
	Rail           string         `json:"rail"`
	Owner          string         `json:"owner"`
	DueAt          time.Time      `json:"due_at"`
	Amount         int64          `json:"amount,string"`
	Currency       string         `json:"currency"`
	// FirstOutcome is approved, declined, error, missed or pending.
	FirstOutcome string     `json:"first_outcome"`
	Outcome      string     `json:"outcome"`
	MissedAt     *time.Time `json:"missed_at,omitempty"`
	MissReason   string     `json:"miss_reason,omitempty"`
	CollectedAt  *time.Time `json:"collected_at,omitempty"`
	// RecoveredBy is what collected a cycle whose first outcome failed:
	// dunning_retry, customer_retry, updated_card or late_provider_charge.
	RecoveredBy string    `json:"recovered_by,omitempty"`
	ClosesAt    time.Time `json:"closes_at"`
	// Attempts are the cycle's attempts, oldest first (GetRebillCycle only).
	Attempts []PaymentAttempt `json:"attempts,omitempty"`
}

// RebillCycleFilter selects cycles, latest due first; every field is
// optional, and a list matches any of its values. DueSince and DueUntil bound
// due_at to [DueSince, DueUntil).
type RebillCycleFilter struct {
	PageOptions
	Owner, FirstOutcome, MissReason, Outcome []string
	// PSPID is the provider account's plain UUID.
	PSPID              string
	SubscriptionID     SubscriptionID
	DueSince, DueUntil time.Time
}

// ListPaymentAttempts lists the merchant's payment attempts, newest first.
func (c *Client) ListPaymentAttempts(ctx context.Context, filter PaymentAttemptFilter, requestOptions ...RequestOption) (*Page[PaymentAttempt], error) {
	q := pageQuery(filter.PageOptions)
	setQuery(q, map[string]string{"kind": commaList(filter.Kind), "owner": commaList(filter.Owner), "category": commaList(filter.Category), "reason": commaList(filter.Reason),
		"response_code": commaList(filter.ResponseCode), "card_entry": commaList(filter.CardEntry), "source": commaList(filter.Source),
		"observed_via": commaList(filter.ObservedVia), "avs_result": commaList(filter.AVSResult), "cvv_result": commaList(filter.CVVResult),
		"psp_id": filter.PSPID, "customer_id": filter.CustomerID,
		"checkout_id": filter.CheckoutID, "subscription_id": filter.SubscriptionID.String(), "cycle_id": filter.CycleID.String(),
		"since": timeQuery(filter.Since), "until": timeQuery(filter.Until)})
	var out Page[PaymentAttempt]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/payment-attempts?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPaymentAttempt reads one payment attempt.
func (c *Client) GetPaymentAttempt(ctx context.Context, id PaymentAttemptID, requestOptions ...RequestOption) (*PaymentAttempt, error) {
	attempt, err := requireTypedID("payment_attempt_id", id)
	if err != nil {
		return nil, err
	}
	var out PaymentAttempt
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/payment-attempts/"+attempt, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRebillCycles lists the merchant's rebill cycles, latest due first.
func (c *Client) ListRebillCycles(ctx context.Context, filter RebillCycleFilter, requestOptions ...RequestOption) (*Page[RebillCycle], error) {
	q := pageQuery(filter.PageOptions)
	setQuery(q, map[string]string{"owner": commaList(filter.Owner), "first_outcome": commaList(filter.FirstOutcome), "miss_reason": commaList(filter.MissReason),
		"outcome": commaList(filter.Outcome), "psp_id": filter.PSPID, "subscription_id": filter.SubscriptionID.String(),
		"due_since": timeQuery(filter.DueSince), "due_until": timeQuery(filter.DueUntil)})
	var out Page[RebillCycle]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/rebill-cycles?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRebillCycle reads one rebill cycle with its attempts.
func (c *Client) GetRebillCycle(ctx context.Context, id RebillCycleID, requestOptions ...RequestOption) (*RebillCycle, error) {
	cycle, err := requireTypedID("rebill_cycle_id", id)
	if err != nil {
		return nil, err
	}
	var out RebillCycle
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
