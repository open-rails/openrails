package reconcile

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// stripeSubPage builds one /v1/subscriptions list page.
func stripeSubPage(hasMore bool, items ...string) string {
	body := `{"object":"list","has_more":`
	if hasMore {
		body += "true"
	} else {
		body += "false"
	}
	body += `,"data":[`
	for i, it := range items {
		if i > 0 {
			body += ","
		}
		body += it
	}
	return body + `]}`
}

const stripeSubActive = `{
	"id": "sub_active1",
	"object": "subscription",
	"status": "active",
	"customer": "cus_A",
	"currency": "usd",
	"cancel_at_period_end": false,
	"items": {"data": [{
		"current_period_start": 1780709768,
		"current_period_end": 1783301768,
		"price": {"id": "price_X", "unit_amount": 11900, "currency": "usd"}
	}]},
	"default_payment_method": {
		"id": "pm_1", "object": "payment_method",
		"card": {"last4": "4242", "exp_month": 7, "exp_year": 2030}
	}
}`

const stripeSubCanceled = `{
	"id": "sub_gone1",
	"object": "subscription",
	"status": "canceled",
	"customer": "cus_B",
	"currency": "usd",
	"items": {"data": [{
		"price": {"id": "price_Y", "unit_amount": 999, "currency": "usd"}
	}]},
	"default_payment_method": null
}`

func newStripeTestServer(t *testing.T, paths *[]string) *httptest.Server {
	t.Helper()
	page := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "Bearer sk_test_x", r.Header.Get("Authorization"))
		if paths != nil {
			*paths = append(*paths, r.URL.Path+"?"+r.URL.RawQuery)
		}
		switch r.URL.Path {
		case "/v1/subscriptions":
			// Two pages to exercise cursor pagination.
			if page == 0 {
				page++
				require.Empty(t, r.URL.Query().Get("starting_after"))
				fmt.Fprint(w, stripeSubPage(true, stripeSubActive))
				return
			}
			require.Equal(t, "sub_active1", r.URL.Query().Get("starting_after"))
			fmt.Fprint(w, stripeSubPage(false, stripeSubCanceled))
		case "/v1/charges":
			fmt.Fprint(w, `{"object":"list","has_more":false,"data":[
				{"id":"ch_ok","amount":11900,"currency":"usd","created":1780709768,"paid":true,"captured":true,"status":"succeeded","invoice":"in_1"},
				{"id":"ch_fail","amount":999,"currency":"usd","created":1780809768,"paid":false,"captured":false,"status":"failed","failure_code":"card_declined","failure_message":"Your card was declined."}
			]}`)
		case "/v1/refunds":
			fmt.Fprint(w, `{"object":"list","has_more":false,"data":[
				{"id":"re_1","amount":11900,"currency":"usd","created":1780909768,"status":"succeeded","charge":"ch_ok"}
			]}`)
		case "/v1/disputes":
			fmt.Fprint(w, `{"object":"list","has_more":false,"data":[
				{"id":"dp_1","amount":11900,"currency":"usd","created":1781009768,"status":"needs_response","charge":"ch_ok"}
			]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestStripeFetcher_Fetch(t *testing.T) {
	t.Parallel()

	var paths []string
	server := newStripeTestServer(t, &paths)
	fetcher := &StripeFetcher{
		SecretKey:  "sk_test_x",
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	}

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	snap, err := fetcher.Fetch(context.Background(), FetchParams{Since: since, Until: until})
	require.NoError(t, err)

	require.Equal(t, ProviderStripe, snap.Provider)
	require.True(t, snap.Capabilities.Vault)
	require.True(t, snap.Capabilities.Chargebacks)
	require.True(t, snap.Coverage.SubscriptionsExhaustive)
	require.True(t, snap.Coverage.TransactionsExhaustive)
	require.True(t, snap.Coverage.TransactionsPaginatedComplete)
	require.NotNil(t, snap.Coverage.TransactionWindowSince)
	require.NotNil(t, snap.Coverage.TransactionWindowUntil)
	require.True(t, snap.Coverage.TransactionWindowSince.Equal(since))
	require.True(t, snap.Coverage.TransactionWindowUntil.Equal(until))

	// Subscriptions across both pages.
	require.Len(t, snap.Subscriptions, 2)
	active := snap.Subscriptions[0]
	require.Equal(t, "sub_active1", active.RailSubscriptionID)
	require.Equal(t, SubscriptionStatusActive, active.Status)
	require.Equal(t, "active", active.RawStatus)
	require.Equal(t, "cus_A", active.CustomerID)
	require.Equal(t, "price_X", active.PlanID)
	require.Equal(t, int64(11900), active.AmountCents)
	require.Equal(t, "USD", active.Currency)
	// Item-level period dates picked up.
	require.NotNil(t, active.LastBilledAt)
	require.Equal(t, time.Unix(1780709768, 0).UTC(), *active.LastBilledAt)
	require.NotNil(t, active.NextBillingAt)
	require.Equal(t, time.Unix(1783301768, 0).UTC(), *active.NextBillingAt)

	cancelled := snap.Subscriptions[1]
	require.Equal(t, SubscriptionStatusCancelled, cancelled.Status)
	require.Equal(t, "canceled", cancelled.RawStatus)

	// Vault: only the sub with an expanded default_payment_method.
	require.Len(t, snap.PaymentMethods, 1)
	require.Equal(t, "cus_A", snap.PaymentMethods[0].RailCustomerRef)
	require.Equal(t, "4242", snap.PaymentMethods[0].CardLast4)
	require.Equal(t, "0730", snap.PaymentMethods[0].CardExpiry)

	// Transactions: sale, decline, refund, chargeback.
	require.Len(t, snap.Transactions, 4)
	require.Equal(t, TransactionTypeSale, snap.Transactions[0].Type)
	require.True(t, snap.Transactions[0].Success)
	require.Equal(t, int64(11900), snap.Transactions[0].AmountCents)

	decline := snap.Transactions[1]
	require.Equal(t, TransactionTypeDecline, decline.Type)
	require.False(t, decline.Success)
	require.Contains(t, decline.DeclineReason, "card_declined")

	require.Equal(t, TransactionTypeRefund, snap.Transactions[2].Type)
	require.Equal(t, TransactionTypeChargeback, snap.Transactions[3].Type)
	require.Equal(t, time.Unix(1781009768, 0).UTC(), snap.Transactions[3].OccurredAt)

	// Date window applied to event endpoints, not the subscription roster.
	for _, p := range paths {
		if len(p) >= len("/v1/subscriptions") && p[:len("/v1/subscriptions")] == "/v1/subscriptions" {
			require.NotContains(t, p, "created%5Bgte%5D")
			require.Contains(t, p, "status=all")
			continue
		}
		require.Contains(t, p, "created%5Bgte%5D="+fmt.Sprint(since.Unix()))
		require.Contains(t, p, "created%5Blte%5D="+fmt.Sprint(until.Unix()))
	}
}

func TestStripeFetcher_SingleSubscriptionFilter(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/subscriptions/sub_active1":
			fmt.Fprint(w, stripeSubActive)
		case "/v1/charges", "/v1/refunds", "/v1/disputes":
			fmt.Fprint(w, `{"object":"list","has_more":false,"data":[]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	fetcher := &StripeFetcher{SecretKey: "sk_test_x", BaseURL: server.URL, HTTPClient: server.Client()}
	snap, err := fetcher.Fetch(context.Background(), FetchParams{SubscriptionID: "sub_active1"})
	require.NoError(t, err)
	require.Len(t, snap.Subscriptions, 1)
	require.Equal(t, "sub_active1", snap.Subscriptions[0].RailSubscriptionID)
}

func TestStripeFetcher_APIErrorSurfaced(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Invalid API Key provided"}}`)
	}))
	t.Cleanup(server.Close)

	fetcher := &StripeFetcher{SecretKey: "sk_bad", BaseURL: server.URL, HTTPClient: server.Client()}
	_, err := fetcher.Fetch(context.Background(), FetchParams{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Invalid API Key")
}

// or#842: SubscriptionsExhaustive is an ABSENCE PROOF — it authorizes cancelling
// every local subscription missing from the roster and flips the §3.2
// confirmed-absence gate for the whole merchant. Stripe used to stamp it before
// the list call had even run, so a 200 with an empty `data` (a key rotated onto
// a sibling account, a restricted key, an incident) was certified as "this
// merchant has no subscribers".
func TestStripeFetcher_EmptyRosterIsNotExhaustive(t *testing.T) {
	t.Parallel()

	newFetcher := func(t *testing.T, subsPage string) *StripeFetcher {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/subscriptions" {
				fmt.Fprint(w, subsPage)
				return
			}
			fmt.Fprint(w, `{"object":"list","has_more":false,"data":[]}`)
		}))
		t.Cleanup(server.Close)
		return &StripeFetcher{SecretKey: "sk_test_x", BaseURL: server.URL, HTTPClient: server.Client()}
	}

	empty, err := newFetcher(t, stripeSubPage(false)).Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Empty(t, empty.Subscriptions)
	require.False(t, empty.Coverage.SubscriptionsExhaustive,
		"an empty roster was stamped exhaustive — it would cancel the merchant's entire book")

	full, err := newFetcher(t, stripeSubPage(false, stripeSubActive)).Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.True(t, full.Coverage.SubscriptionsExhaustive, "a non-empty completed roster still proves absence")

	// A customer-filtered roster is exhaustive for that customer, never for the
	// merchant's book — and only the book-wide claim is what the gate reads.
	scoped, err := newFetcher(t, stripeSubPage(false, stripeSubActive)).
		Fetch(context.Background(), FetchParams{CustomerID: "cus_A"})
	require.NoError(t, err)
	require.False(t, scoped.Coverage.SubscriptionsExhaustive,
		"a customer-scoped roster proves nothing about the subscriptions of every OTHER customer")
}
