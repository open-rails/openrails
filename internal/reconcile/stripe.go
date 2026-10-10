package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
)

// StripeFetcher pulls Stripe state with raw GETs, always through the stripeapi
// read-only transport so any write on this path fails before the network.
// Sources: /v1/subscriptions (status=all), /v1/charges, /v1/refunds and
// /v1/disputes over [Since,Until].
//
// Quirks: current_period_start/end live on the subscription item on current
// API versions (the item wins when the envelope is zero). A charge's
// subscription id needs an invoice expansion, so SubscriptionID stays empty and
// the invoice id stays in Raw. Vault entries come from the expanded
// default_payment_method, not an N+1 /v1/payment_methods sweep.
type StripeFetcher struct {
	SecretKey string
	// BaseURL defaults to https://api.stripe.com; overridable for tests.
	BaseURL string
	// HTTPClient defaults to stripeapi.ReadOnlyClient.
	HTTPClient *http.Client
	// PageLimit is the per-page list size (default and Stripe max: 100).
	PageLimit int
}

// NewStripeFetcher builds a fetcher from a Stripe secret key, on the
// unconditionally write-blocked stripeapi client.
func NewStripeFetcher(secretKey string) *StripeFetcher {
	return &StripeFetcher{
		SecretKey:  secretKey,
		HTTPClient: stripeapi.ReadOnlyClient(30 * time.Second),
	}
}

func (f *StripeFetcher) Name() string { return string(ProviderStripe) }

func (f *StripeFetcher) Capabilities() Capabilities {
	return Capabilities{
		Subscriptions: true,
		Transactions:  true,
		Refunds:       true,
		Chargebacks:   true,
		Vault:         true,
	}
}

func (f *StripeFetcher) Fetch(ctx context.Context, params FetchParams) (*RemoteSnapshot, error) {
	snap := &RemoteSnapshot{
		Provider:     ProviderStripe,
		FetchedAt:    fetchObservedAt(params),
		Capabilities: f.Capabilities(),
	}
	snap.Coverage.TransactionsExhaustive = true
	snap.Coverage.TransactionsPaginatedComplete = true
	snap.Coverage.TransactionWindowSince = timePtrIfSet(params.Since)
	snap.Coverage.TransactionWindowUntil = timePtrIfSet(params.Until)

	subPages, err := f.listSubscriptions(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("stripe subscriptions: %w", err)
	}
	for _, env := range subPages {
		sub, vault := normalizeStripeSubscription(env)
		snap.Subscriptions = append(snap.Subscriptions, sub)
		if vault != nil {
			snap.PaymentMethods = append(snap.PaymentMethods, *vault)
		}
	}
	// "Exhaustive" is an absence proof: it sets the confirmed-absence gate and
	// authorizes cancelling every local subscription missing from this list, so
	// it is decided from what came back. An empty 200 is indistinguishable from
	// another account's complete roster (rotated or restricted key, incident),
	// and a customer-filtered roster proves nothing about the book.
	if params.SubscriptionID == "" && params.CustomerID == "" && len(snap.Subscriptions) > 0 {
		snap.Coverage.SubscriptionsExhaustive = true
	}

	charges, err := f.listRaw(ctx, "/v1/charges", params, nil)
	if err != nil {
		return nil, fmt.Errorf("stripe charges: %w", err)
	}
	for _, obj := range charges {
		snap.Transactions = append(snap.Transactions, normalizeStripeCharge(obj))
	}

	refunds, err := f.listRaw(ctx, "/v1/refunds", params, nil)
	if err != nil {
		return nil, fmt.Errorf("stripe refunds: %w", err)
	}
	for _, obj := range refunds {
		snap.Transactions = append(snap.Transactions, normalizeStripeRefund(obj))
	}

	// /v1/disputes does not accept a customer filter.
	disputes, err := f.listRaw(ctx, "/v1/disputes", FetchParams{Since: params.Since, Until: params.Until}, nil)
	if err != nil {
		return nil, fmt.Errorf("stripe disputes: %w", err)
	}
	for _, obj := range disputes {
		snap.Transactions = append(snap.Transactions, normalizeStripeDispute(obj))
	}

	return snap, nil
}

func (f *StripeFetcher) baseURL() string {
	if f.BaseURL != "" {
		return strings.TrimRight(f.BaseURL, "/")
	}
	return "https://api.stripe.com"
}

func (f *StripeFetcher) client() *http.Client {
	if f.HTTPClient != nil {
		return f.HTTPClient
	}
	return stripeapi.ReadOnlyClient(30 * time.Second)
}

func (f *StripeFetcher) pageLimit() int {
	if f.PageLimit > 0 && f.PageLimit <= 100 {
		return f.PageLimit
	}
	return 100
}

// stripeListEnvelope is the generic Stripe list page shape; items are kept
// raw so each normalizer extracts its own fields and the full object is
// preserved for forensics.
type stripeListEnvelope struct {
	HasMore bool              `json:"has_more"`
	Data    []json.RawMessage `json:"data"`
}

// stripeID pulls the "id" out of a raw Stripe object for cursoring.
func stripeID(obj json.RawMessage) string {
	var v struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(obj, &v)
	return v.ID
}

// listRaw cursor-paginates a Stripe list endpoint, applying created[gte/lte]
// from Since/Until and a customer filter when set. extra adds endpoint-
// specific query params.
func (f *StripeFetcher) listRaw(ctx context.Context, path string, params FetchParams, extra url.Values) ([]json.RawMessage, error) {
	var (
		all           []json.RawMessage
		startingAfter string
	)
	for {
		values := url.Values{}
		for k, vs := range extra {
			for _, v := range vs {
				values.Add(k, v)
			}
		}
		values.Set("limit", strconv.Itoa(f.pageLimit()))
		if !params.Since.IsZero() {
			values.Set("created[gte]", strconv.FormatInt(params.Since.Unix(), 10))
		}
		if !params.Until.IsZero() {
			values.Set("created[lte]", strconv.FormatInt(params.Until.Unix(), 10))
		}
		if params.CustomerID != "" {
			values.Set("customer", params.CustomerID)
		}
		if startingAfter != "" {
			values.Set("starting_after", startingAfter)
		}

		body, err := f.get(ctx, path+"?"+values.Encode())
		if err != nil {
			return nil, err
		}
		var page stripeListEnvelope
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("parse %s page: %w", path, err)
		}
		all = append(all, page.Data...)
		if !page.HasMore || len(page.Data) == 0 {
			break
		}
		startingAfter = stripeID(page.Data[len(page.Data)-1])
		if startingAfter == "" {
			return nil, fmt.Errorf("stripe %s page has has_more=true but last object has no id", path)
		}
	}
	return all, nil
}

// listSubscriptions pages /v1/subscriptions?status=all with the price, default
// payment method and latest invoice expanded; a SubscriptionID filter is a
// single GET. The roster is point-in-time state, so it takes no created window.
func (f *StripeFetcher) listSubscriptions(ctx context.Context, params FetchParams) ([]json.RawMessage, error) {
	if params.SubscriptionID != "" {
		body, err := f.get(ctx, "/v1/subscriptions/"+url.PathEscape(params.SubscriptionID)+"?expand[]=default_payment_method&expand[]=latest_invoice")
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{body}, nil
	}

	extra := url.Values{}
	extra.Set("status", "all")
	extra.Add("expand[]", "data.default_payment_method")
	extra.Add("expand[]", "data.latest_invoice")
	// No created window for the roster: pass only the customer filter.
	return f.listRaw(ctx, "/v1/subscriptions", FetchParams{CustomerID: params.CustomerID}, extra)
}

func (f *StripeFetcher) get(ctx context.Context, pathAndQuery string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL()+pathAndQuery, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.SecretKey)

	resp, err := f.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Error.Message != "" {
			return nil, fmt.Errorf("stripe API error (%d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		return nil, fmt.Errorf("stripe API error (%d)", resp.StatusCode)
	}
	return body, nil
}

type stripeSubscriptionJSON struct {
	ID                 string `json:"id"`
	Status             string `json:"status"`
	Customer           string `json:"customer"`
	CancelAtPeriodEnd  bool   `json:"cancel_at_period_end"`
	CurrentPeriodStart int64  `json:"current_period_start"`
	CurrentPeriodEnd   int64  `json:"current_period_end"`
	Currency           string `json:"currency"`
	Items              struct {
		Data []struct {
			CurrentPeriodStart int64 `json:"current_period_start"`
			CurrentPeriodEnd   int64 `json:"current_period_end"`
			Price              struct {
				ID         string `json:"id"`
				UnitAmount int64  `json:"unit_amount"`
				Currency   string `json:"currency"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
	DefaultPaymentMethod json.RawMessage `json:"default_payment_method"`
	LatestInvoice        json.RawMessage `json:"latest_invoice"`
}

type stripePaymentMethodJSON struct {
	ID   string `json:"id"`
	Card struct {
		Last4    string `json:"last4"`
		ExpMonth int    `json:"exp_month"`
		ExpYear  int    `json:"exp_year"`
	} `json:"card"`
}

// stripeRemoteStatus maps a Stripe-owned subscription's status. Stripe runs
// the retries of the subscriptions it owns, so its word is final: unpaid and
// paused grant nothing, and past_due is live only while Stripe will still
// retry the open invoice.
func stripeRemoteStatus(status string, retryExhausted bool) SubscriptionStatus {
	switch status {
	case "active", "trialing":
		return SubscriptionStatusActive
	case "canceled":
		return SubscriptionStatusCanceled
	case "incomplete_expired", "unpaid", "paused":
		return SubscriptionStatusExpired
	case "past_due":
		if retryExhausted {
			return SubscriptionStatusExpired
		}
		return SubscriptionStatusPastDue
	default: // incomplete, anything new
		return SubscriptionStatusUnknown
	}
}

// stripeInvoiceRetryExhausted reports an expanded latest invoice that is open
// with no further payment attempt scheduled: Stripe has stopped retrying. An
// unexpanded (string) invoice proves nothing.
func stripeInvoiceRetryExhausted(raw json.RawMessage) bool {
	var inv struct {
		ID                 string `json:"id"`
		Status             string `json:"status"`
		NextPaymentAttempt int64  `json:"next_payment_attempt"`
	}
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &inv) != nil || inv.ID == "" {
		return false
	}
	return inv.Status == "open" && inv.NextPaymentAttempt == 0
}

func normalizeStripeSubscription(obj json.RawMessage) (RemoteSubscription, *RemotePaymentMethod) {
	var s stripeSubscriptionJSON
	_ = json.Unmarshal(obj, &s)

	sub := RemoteSubscription{
		RailSubscriptionID: s.ID,
		Status:             stripeRemoteStatus(s.Status, stripeInvoiceRetryExhausted(s.LatestInvoice)),
		RawStatus:          s.Status,
		CustomerID:         s.Customer,
		Currency:           strings.ToUpper(s.Currency),
		Raw:                obj,
	}
	periodStart, periodEnd := s.CurrentPeriodStart, s.CurrentPeriodEnd
	if len(s.Items.Data) > 0 {
		item := s.Items.Data[0]
		sub.PlanID = item.Price.ID
		sub.AmountCents = item.Price.UnitAmount
		if sub.Currency == "" {
			sub.Currency = strings.ToUpper(item.Price.Currency)
		}
		if periodStart == 0 {
			periodStart = item.CurrentPeriodStart
		}
		if periodEnd == 0 {
			periodEnd = item.CurrentPeriodEnd
		}
	}
	if periodStart > 0 {
		t := time.Unix(periodStart, 0).UTC()
		sub.LastBilledAt = &t
	}
	if periodEnd > 0 {
		t := time.Unix(periodEnd, 0).UTC()
		sub.NextBillingAt = &t
	}

	var vault *RemotePaymentMethod
	if len(s.DefaultPaymentMethod) > 0 && string(s.DefaultPaymentMethod) != "null" {
		var pm stripePaymentMethodJSON
		if err := json.Unmarshal(s.DefaultPaymentMethod, &pm); err == nil && pm.ID != "" {
			entry := RemotePaymentMethod{
				RailCustomerRef: s.Customer,
				CardLast4:       pm.Card.Last4,
				Raw:             s.DefaultPaymentMethod,
			}
			if pm.Card.ExpMonth > 0 && pm.Card.ExpYear > 0 {
				entry.CardExpiry = fmt.Sprintf("%02d%02d", pm.Card.ExpMonth, pm.Card.ExpYear%100)
			}
			vault = &entry
		}
	}
	return sub, vault
}

func normalizeStripeCharge(obj json.RawMessage) RemoteTransaction {
	var c struct {
		ID             string `json:"id"`
		Amount         int64  `json:"amount"`
		Currency       string `json:"currency"`
		Created        int64  `json:"created"`
		Paid           bool   `json:"paid"`
		Status         string `json:"status"` // succeeded | pending | failed
		Captured       bool   `json:"captured"`
		FailureCode    string `json:"failure_code"`
		FailureMessage string `json:"failure_message"`
	}
	_ = json.Unmarshal(obj, &c)

	txn := RemoteTransaction{
		TransactionID: c.ID,
		Type:          TransactionTypeSale,
		Success:       c.Status == "succeeded",
		AmountCents:   c.Amount,
		Currency:      strings.ToUpper(c.Currency),
		Raw:           obj,
	}
	if c.Created > 0 {
		txn.OccurredAt = time.Unix(c.Created, 0).UTC()
	}
	if c.Status == "failed" {
		txn.Type = TransactionTypeDecline
		txn.DeclineReason = strings.TrimSpace(strings.TrimSpace(c.FailureCode + " " + c.FailureMessage))
		txn.DeclineCode = strings.TrimSpace(c.FailureCode)
	} else if c.Status == "succeeded" && !c.Captured {
		txn.Type = TransactionTypeAuth
	}
	return txn
}

func normalizeStripeRefund(obj json.RawMessage) RemoteTransaction {
	var r struct {
		ID       string `json:"id"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Created  int64  `json:"created"`
		Status   string `json:"status"` // succeeded | pending | failed | canceled
	}
	_ = json.Unmarshal(obj, &r)

	txn := RemoteTransaction{
		TransactionID: r.ID,
		Type:          TransactionTypeRefund,
		Success:       r.Status == "succeeded",
		AmountCents:   r.Amount,
		Currency:      strings.ToUpper(r.Currency),
		Raw:           obj,
	}
	if r.Created > 0 {
		txn.OccurredAt = time.Unix(r.Created, 0).UTC()
	}
	return txn
}

func normalizeStripeDispute(obj json.RawMessage) RemoteTransaction {
	var d struct {
		ID       string `json:"id"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Created  int64  `json:"created"`
		Status   string `json:"status"`
	}
	_ = json.Unmarshal(obj, &d)

	txn := RemoteTransaction{
		TransactionID: d.ID,
		Type:          TransactionTypeChargeback,
		// A dispute exists => the chargeback event happened, whatever its
		// current lifecycle status; the status is preserved in Raw.
		Success:     true,
		AmountCents: d.Amount,
		Currency:    strings.ToUpper(d.Currency),
		Raw:         obj,
	}
	if d.Created > 0 {
		txn.OccurredAt = time.Unix(d.Created, 0).UTC()
	}
	return txn
}
