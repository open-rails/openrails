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

// StripeFetcher pulls Stripe state via raw GETs against the REST API (house
// style: no stripe-go SDK), always through the stripeapi read-only choke
// transport so any write sneaking onto this path fails loudly before the
// network.
//
// Sources: /v1/subscriptions (status=all, cursor-paginated), /v1/charges,
// /v1/refunds, /v1/disputes over [Since,Until]. Full capabilities.
//
// Provider quirks:
//   - On current API versions current_period_start/end live on the
//     subscription ITEM, not the subscription envelope; both locations are
//     read (item wins when the envelope is zero).
//   - charge.invoice is set for subscription-driven charges but the
//     subscription id itself requires an invoice expansion; SubscriptionID is
//     left empty and the invoice id is preserved in Raw.
//   - Vault entries are derived from the subscriptions' default_payment_method
//     expansion (card last4/exp) rather than a separate per-customer
//     /v1/payment_methods sweep (which is a per-customer API, N+1 over the
//     roster).
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
		FetchedAt:    time.Now().UTC(),
		Capabilities: f.Capabilities(),
	}
	if params.SubscriptionID == "" {
		snap.Coverage.SubscriptionsExhaustive = true
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
			snap.VaultEntries = append(snap.VaultEntries, *vault)
		}
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

// listSubscriptions pages /v1/subscriptions?status=all with the price and
// default payment method expanded. A SubscriptionID filter short-circuits to
// the single-object GET. Note: /v1/subscriptions does not date-filter on
// created here — the subscription roster is point-in-time state, not events.
func (f *StripeFetcher) listSubscriptions(ctx context.Context, params FetchParams) ([]json.RawMessage, error) {
	if params.SubscriptionID != "" {
		body, err := f.get(ctx, "/v1/subscriptions/"+url.PathEscape(params.SubscriptionID)+"?expand[]=default_payment_method")
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{body}, nil
	}

	extra := url.Values{}
	extra.Set("status", "all")
	extra.Add("expand[]", "data.default_payment_method")
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

// --- normalizers ---

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
}

type stripePaymentMethodJSON struct {
	ID   string `json:"id"`
	Card struct {
		Last4    string `json:"last4"`
		ExpMonth int    `json:"exp_month"`
		ExpYear  int    `json:"exp_year"`
	} `json:"card"`
}

// normalizeStripeStatus maps Stripe subscription statuses onto the normalized
// enum.
func normalizeStripeStatus(s string) SubscriptionStatus {
	switch s {
	case "active", "trialing":
		return SubscriptionStatusActive
	case "canceled":
		return SubscriptionStatusCancelled
	case "incomplete_expired":
		return SubscriptionStatusExpired
	case "past_due", "unpaid":
		return SubscriptionStatusPastDue
	default: // incomplete, paused, anything new
		return SubscriptionStatusUnknown
	}
}

func normalizeStripeSubscription(obj json.RawMessage) (RemoteSubscription, *RemoteVaultEntry) {
	var s stripeSubscriptionJSON
	_ = json.Unmarshal(obj, &s)

	sub := RemoteSubscription{
		RailSubscriptionID: s.ID,
		Status:             normalizeStripeStatus(s.Status),
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

	var vault *RemoteVaultEntry
	if len(s.DefaultPaymentMethod) > 0 && string(s.DefaultPaymentMethod) != "null" {
		var pm stripePaymentMethodJSON
		if err := json.Unmarshal(s.DefaultPaymentMethod, &pm); err == nil && pm.ID != "" {
			entry := RemoteVaultEntry{
				CustomerVaultID: s.Customer,
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
