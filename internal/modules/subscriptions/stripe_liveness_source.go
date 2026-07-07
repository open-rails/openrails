package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/railresolve"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// Stripe side of the subscription liveness sync (#367): a read-only,
// per-subscription provider-truth probe — GET /v1/subscriptions/{id} with the
// latest invoice expanded. Stripe runs its own dunning and retries webhooks
// for days, so this is lower urgency than the NMI probe, but it repairs long
// webhook outages the same way.

// StripeLivenessRecord is the normalized remote-truth view of one Stripe
// subscription for liveness classification. Found=false means Stripe no
// longer knows the subscription (deleted/never existed).
type StripeLivenessRecord struct {
	Found              bool
	Status             string
	CancelAtPeriodEnd  bool
	CanceledAt         time.Time
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	// CustomerID is Stripe's cus_… id; Metadata is the subscription metadata
	// (checkout stamps user_id / internal_price_id / checkout_session_id there).
	// PriceID is the first item's Stripe price id. Fetch-sourced identity for
	// the #684 converge path — never read from webhook payloads.
	CustomerID string
	Metadata   map[string]string
	PriceID    string
	// Latest invoice evidence (zero values when no invoice is attached).
	// LatestInvoiceTransactionID follows the webhook's transaction-id
	// precedence (charge > payment_intent > invoice id) so a probe-driven
	// repair and a late-arriving invoice.paid webhook dedupe on the same key.
	LatestInvoicePaid          bool
	LatestInvoiceID            string
	LatestInvoiceTransactionID string
	LatestInvoiceAmountPaid    int64
	LatestInvoiceAmountDue     int64
	LatestInvoiceCurrency      string
	LatestInvoiceCreated       time.Time
	LatestInvoiceBillingReason string
}

// StripeLivenessProber probes one remote Stripe subscription. Interface so
// the liveness worker can be tested without the live Stripe API.
type StripeLivenessProber interface {
	ProbeSubscription(ctx context.Context, railSubscriptionID string) (StripeLivenessRecord, error)
}

// HTTPStripeLivenessProber is the production prober over the Stripe REST API.
type HTTPStripeLivenessProber struct {
	SecretKey  string
	HTTPClient *http.Client
	// BaseURL overrides https://api.stripe.com for tests.
	BaseURL string
}

// NewStripeLivenessProber builds a prober from the ctx merchant's armed
// Stripe account, reusing RequireStripeSecretKey for auth. Returns an error
// when Stripe is not armed — callers skip the Stripe slice of the cohort.
func NewStripeLivenessProber(ctx context.Context, src railresolve.Source) (*HTTPStripeLivenessProber, error) {
	_, secretKey, err := RequireStripeSecretKey(ctx, src)
	if err != nil {
		return nil, err
	}
	return &HTTPStripeLivenessProber{SecretKey: secretKey}, nil
}

type stripeLivenessSubscriptionEnvelope struct {
	ID                 string            `json:"id"`
	Status             string            `json:"status"`
	CancelAtPeriodEnd  bool              `json:"cancel_at_period_end"`
	CanceledAt         int64             `json:"canceled_at"`
	CurrentPeriodStart int64             `json:"current_period_start"`
	CurrentPeriodEnd   int64             `json:"current_period_end"`
	Customer           string            `json:"customer"`
	Metadata           map[string]string `json:"metadata"`
	Items              struct {
		Data []struct {
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
	LatestInvoice struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		Paid          bool   `json:"paid"`
		Charge        string `json:"charge"`
		PaymentIntent string `json:"payment_intent"`
		AmountPaid    int64  `json:"amount_paid"`
		AmountDue     int64  `json:"amount_due"`
		Currency      string `json:"currency"`
		Created       int64  `json:"created"`
		BillingReason string `json:"billing_reason"`
	} `json:"latest_invoice"`
}

// parseStripeLivenessSubscription decodes one expanded subscription envelope
// into the normalized record. Split from the HTTP path for unit testing.
func parseStripeLivenessSubscription(body []byte) (StripeLivenessRecord, error) {
	var env stripeLivenessSubscriptionEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return StripeLivenessRecord{}, fmt.Errorf("stripe subscription parse failed: %w", err)
	}
	rec := StripeLivenessRecord{
		Found:             strings.TrimSpace(env.ID) != "",
		Status:            strings.TrimSpace(strings.ToLower(env.Status)),
		CancelAtPeriodEnd: env.CancelAtPeriodEnd,
		CustomerID:        strings.TrimSpace(env.Customer),
		Metadata:          env.Metadata,
	}
	if env.CanceledAt > 0 {
		rec.CanceledAt = time.Unix(env.CanceledAt, 0).UTC()
	}
	if env.CurrentPeriodStart > 0 {
		rec.CurrentPeriodStart = time.Unix(env.CurrentPeriodStart, 0).UTC()
	}
	if env.CurrentPeriodEnd > 0 {
		rec.CurrentPeriodEnd = time.Unix(env.CurrentPeriodEnd, 0).UTC()
	}
	if len(env.Items.Data) > 0 {
		rec.PriceID = strings.TrimSpace(env.Items.Data[0].Price.ID)
	}
	inv := env.LatestInvoice
	if strings.TrimSpace(inv.ID) != "" {
		rec.LatestInvoicePaid = inv.Paid || strings.EqualFold(strings.TrimSpace(inv.Status), "paid")
		rec.LatestInvoiceID = strings.TrimSpace(inv.ID)
		rec.LatestInvoiceTransactionID = normalize.FirstNonEmpty(inv.Charge, inv.PaymentIntent, inv.ID)
		rec.LatestInvoiceAmountPaid = inv.AmountPaid
		rec.LatestInvoiceAmountDue = inv.AmountDue
		rec.LatestInvoiceCurrency = strings.TrimSpace(inv.Currency)
		rec.LatestInvoiceBillingReason = strings.TrimSpace(inv.BillingReason)
		if inv.Created > 0 {
			rec.LatestInvoiceCreated = time.Unix(inv.Created, 0).UTC()
		}
	}
	return rec, nil
}

// ProbeSubscription fetches GET /v1/subscriptions/{id}?expand[]=latest_invoice.
// A 404 returns Found=false with a nil error — at Stripe that is an answer
// (remote absent), not a transport failure.
func (p *HTTPStripeLivenessProber) ProbeSubscription(ctx context.Context, railSubscriptionID string) (StripeLivenessRecord, error) {
	id := strings.TrimSpace(railSubscriptionID)
	if id == "" {
		return StripeLivenessRecord{}, errors.New("rail subscription id is required")
	}
	if strings.TrimSpace(p.SecretKey) == "" {
		return StripeLivenessRecord{}, errors.New("stripe secret key is required")
	}

	client := p.HTTPClient
	if client == nil {
		// Read-only by design: the prober only GETs; the write-blocked client
		// keeps any future mutation on this path failing loudly at the choke.
		client = stripeapi.ReadOnlyClient(30 * time.Second)
	}
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = "https://api.stripe.com"
	}

	values := url.Values{}
	values.Set("expand[]", "latest_invoice")
	reqURL := base + "/v1/subscriptions/" + url.PathEscape(id) + "?" + values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return StripeLivenessRecord{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.SecretKey)

	resp, err := client.Do(req)
	if err != nil {
		return StripeLivenessRecord{}, fmt.Errorf("stripe subscription probe failed: %w", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return StripeLivenessRecord{}, fmt.Errorf("read stripe subscription probe response: %w", readErr)
	}
	if resp.StatusCode == http.StatusNotFound {
		return StripeLivenessRecord{Found: false}, nil
	}
	if resp.StatusCode >= 400 {
		msg := ParseStripeAPIError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe subscription probe failed (%d)", resp.StatusCode)
		}
		return StripeLivenessRecord{}, errors.New(msg)
	}
	return parseStripeLivenessSubscription(body)
}
