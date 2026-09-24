package subscriptions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/railresolve"
)

type capturedRequest struct {
	method, path, idem string
	form               url.Values
}

// stripeFake records every request and answers by "METHOD /path".
func stripeFake(t *testing.T, routes map[string]string) (*httptest.Server, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Authorization") != "Bearer sk_test_wire" {
			t.Errorf("unscoped credentials on %s", r.URL.Path)
		}
		mu.Lock()
		got = append(got, capturedRequest{r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"), r.PostForm})
		mu.Unlock()
		body, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), got...)
	}
}

func wireRails() railresolve.FixedSet {
	return railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_wire"}}}
}

func wireService(baseURL string) *StripeService {
	s := &StripeService{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}, Rails: wireRails()}
	s.SetBaseURLForTest(baseURL)
	return s
}

// Customer creation is idempotent on the app user, so retries and parallel
// checkouts cannot mint duplicate Stripe customers.
func TestStripeCreateCustomerWire(t *testing.T) {
	srv, got := stripeFake(t, map[string]string{"POST /v1/customers": `{"id":"cus_created"}`})
	svc := wireService(srv.URL)
	id, err := svc.CreateCustomer(context.Background(), " a@b.com ", " user-123 ")
	require.NoError(t, err)
	require.Equal(t, "cus_created", id)
	require.Equal(t, "customer_create_user-123", got()[0].idem)
	require.Equal(t, "a@b.com", got()[0].form.Get("email"))
	require.Equal(t, "user-123", got()[0].form.Get("metadata[app_user_id]"))

	_, err = svc.CreateCustomer(context.Background(), "a@b.com", "  ")
	require.ErrorContains(t, err, "app_user_id is required")
	require.Len(t, got(), 1)
}

// #671: rail minor units reach the wire as the literal integer; the invoice
// is created first excluding pending items and the item is attached by id, so
// nothing can be swept across operations. Every step has its own key.
func TestStripeCollectInvoiceWire(t *testing.T) {
	params := StripeInvoiceCollectionParams{CustomerID: "cus_1", PaymentMethodID: "pm_1", AmountCents: 1999, Currency: "USD", IdempotencyKey: "idem-1"}
	paid := `{"id":"in_1","status":"paid","amount_paid":1999,"currency":"usd","payment_intent":"pi_1","charge":"ch_1","metadata":{"openrails_collection_key":"idem-1"}}`
	routes := func(finalized string) map[string]string {
		return map[string]string{
			"POST /v1/invoices":               `{"id":"in_1","status":"draft"}`,
			"POST /v1/invoiceitems":           `{"id":"ii_1"}`,
			"POST /v1/invoices/in_1/finalize": finalized,
			"POST /v1/invoices/in_1/pay":      paid,
		}
	}

	t.Run("paid on finalize", func(t *testing.T) {
		srv, got := stripeFake(t, routes(paid))
		result, err := wireService(srv.URL).CollectInvoice(context.Background(), params)
		require.NoError(t, err)
		require.Equal(t, "ch_1", result.ChargeID)
		reqs := got()
		require.Len(t, reqs, 3)
		inv, item, fin := reqs[0], reqs[1], reqs[2]
		require.Equal(t, "/v1/invoices", inv.path)
		require.Equal(t, "exclude", inv.form.Get("pending_invoice_items_behavior"))
		require.Equal(t, "pm_1", inv.form.Get("default_payment_method"))
		require.Equal(t, "false", inv.form.Get("auto_advance"))
		require.Equal(t, "idem-1", inv.form.Get("metadata[openrails_collection_key]"))
		require.Equal(t, "in_1", item.form.Get("invoice"))
		require.Equal(t, []string{"1999"}, item.form["amount"])
		require.Equal(t, []string{"usd"}, item.form["currency"])
		require.Equal(t, []string{"idem-1:invoice", "idem-1:invoice_item", "idem-1:finalize"}, []string{inv.idem, item.idem, fin.idem})
	})

	t.Run("open after finalize is paid explicitly with the frozen card", func(t *testing.T) {
		srv, got := stripeFake(t, routes(`{"id":"in_1","status":"open","metadata":{"openrails_collection_key":"idem-1"}}`))
		_, err := wireService(srv.URL).CollectInvoice(context.Background(), params)
		require.NoError(t, err)
		pay := got()[3]
		require.Equal(t, "/v1/invoices/in_1/pay", pay.path)
		require.Equal(t, "pm_1", pay.form.Get("payment_method"))
		require.Equal(t, "idem-1:pay", pay.idem)
	})

	for name, body := range map[string]string{
		"underpaid":       `{"id":"in_1","status":"paid","amount_paid":1998,"currency":"usd","charge":"ch_1","metadata":{"openrails_collection_key":"idem-1"}}`,
		"other currency":  `{"id":"in_1","status":"paid","amount_paid":1999,"currency":"eur","charge":"ch_1","metadata":{"openrails_collection_key":"idem-1"}}`,
		"other operation": `{"id":"in_1","status":"paid","amount_paid":1999,"currency":"usd","charge":"ch_1","metadata":{"openrails_collection_key":"idem-2"}}`,
		"not paid":        `{"id":"in_1","status":"uncollectible","amount_paid":0,"currency":"usd","metadata":{"openrails_collection_key":"idem-1"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := routes(body)
			r["POST /v1/invoices/in_1/pay"] = body
			srv, _ := stripeFake(t, r)
			_, err := wireService(srv.URL).CollectInvoice(context.Background(), params)
			require.ErrorIs(t, err, ErrStripeReceiptMismatch)
		})
	}

}

// Refund amounts are literal cents, 0 = full refund (amount omitted); a PI
// target is sent as payment_intent; unknown reasons collapse to Stripe's
// generic one; the default key content-addresses (charge, amount, reason)
// and is mirrored into metadata so the refund is re-findable.
func TestStripeCreateRefundWire(t *testing.T) {
	for _, tc := range []struct {
		name       string
		params     RefundParams
		wantAmount []string
		targetKey  string
		reason     string
		idem       string
	}{
		{"partial", RefundParams{ChargeID: "ch_1", Amount: 500, IdempotencyKey: "k"}, []string{"500"}, "charge", "", "k"},
		{"full omits amount", RefundParams{ChargeID: "ch_1", IdempotencyKey: "k"}, nil, "charge", "", "k"},
		{"payment intent target", RefundParams{ChargeID: "pi_1", Amount: 1999, IdempotencyKey: "k"}, []string{"1999"}, "payment_intent", "", "k"},
		{"known reason", RefundParams{ChargeID: "ch_1", Reason: "fraudulent", IdempotencyKey: "k"}, nil, "charge", "fraudulent", "k"},
		{"unknown reason", RefundParams{ChargeID: "ch_1", Reason: "goodwill", IdempotencyKey: "k"}, nil, "charge", "requested_by_customer", "k"},
		{"default key", RefundParams{ChargeID: "ch_1", Amount: 500}, []string{"500"}, "charge", "", "openrails-refund:ch_1:500:unspecified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := stripeFake(t, map[string]string{"POST /v1/refunds": `{"id":"re_1","status":"succeeded","charge":"ch_1"}`})
			svc := &StripeRefundService{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}, Rails: wireRails(), BaseURL: srv.URL}
			result, err := svc.CreateRefund(context.Background(), tc.params)
			require.NoError(t, err)
			require.Equal(t, "re_1", result.ID)
			req := got()[0]
			require.Equal(t, tc.wantAmount, req.form["amount"])
			require.Equal(t, tc.params.ChargeID, req.form.Get(tc.targetKey))
			require.Equal(t, tc.reason, req.form.Get("reason"))
			require.Equal(t, tc.idem, req.idem)
			require.Equal(t, tc.idem, req.form.Get("metadata["+stripeRefundMetadataKey+"]"))
		})
	}
	require.NotEqual(t, StripeRefundIdempotencyKey("ch_1", 500, ""), StripeRefundIdempotencyKey("ch_1", 501, ""))
}

func TestStripeRefundServiceFailsClosed(t *testing.T) {
	ctx := context.Background()
	var nilSvc *StripeRefundService
	_, err := nilSvc.CreateRefund(ctx, RefundParams{ChargeID: "ch_1"})
	require.ErrorContains(t, err, "not initialized")
	keyless := railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{}}}
	for _, svc := range []*StripeRefundService{{}, {Rails: keyless}} {
		_, err = svc.CreateRefund(ctx, RefundParams{ChargeID: "ch_1"})
		require.Error(t, err, "no armed Stripe account")
	}
	_, err = (&StripeRefundService{Rails: wireRails()}).CreateRefund(ctx, RefundParams{ChargeID: " "})
	require.ErrorContains(t, err, "charge_id or payment_intent_id is required")
}

// Only a Stripe charge or PaymentIntent is refundable; metadata wins over the
// transaction id, and a legacy checkout-session id is refused.
func TestResolveStripeRefundTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		txn  string
		meta map[string]any
		want string
	}{
		{"charge metadata wins", "pi_txn", map[string]any{"stripe_charge_id": "ch_1", "stripe_payment_intent_id": "pi_1"}, "ch_1"},
		{"intent metadata", "in_old", map[string]any{"stripe_payment_intent_id": "pi_1"}, "pi_1"},
		{"malformed metadata ignored", "ch_txn", map[string]any{"stripe_charge_id": "in_1", "stripe_payment_intent_id": 42}, "ch_txn"},
		{"transaction charge", "ch_2", nil, "ch_2"},
		{"transaction intent", "pi_2", nil, "pi_2"},
		{"checkout session refused", "cs_old", nil, ""},
		{"invoice refused", "in_1", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveStripeRefundTarget(&models.Payment{ID: uuid.New(), TransactionID: tc.txn, Metadata: tc.meta})
			if tc.want == "" {
				require.ErrorIs(t, err, ErrStripeRefundTargetMissing)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// #268 Model B upgrade: swap the item price, stamp the new internal price for
// future invoice.paid resolution, invoice the proration now and reset the anchor.
func TestStripeUpdateSubscriptionPriceWire(t *testing.T) {
	srv, got := stripeFake(t, map[string]string{"POST /v1/subscriptions/sub_123": `{"id":"sub_123"}`})
	err := wireService(srv.URL).UpdateSubscriptionPrice(context.Background(), "sub_123", "si_item_1", "price_new", "019e5e09-37e6-7ef7-be77-13a9891a13e0", "always_invoice", "now")
	require.NoError(t, err)
	form := got()[0].form
	for k, want := range map[string]string{
		"items[0][id]": "si_item_1", "items[0][price]": "price_new",
		"metadata[internal_price_id]": "019e5e09-37e6-7ef7-be77-13a9891a13e0",
		"proration_behavior":          "always_invoice", "billing_cycle_anchor": "now",
	} {
		require.Equal(t, want, form.Get(k), k)
	}
}

func TestParseStripeLivenessSubscription(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		status   string
		txn      string
		paid     bool
		failed   bool
		periodTo time.Time
	}{
		{"legacy shape, charge wins", `{"id":"sub_1","status":"active","current_period_end":1782592000,"latest_invoice":{"id":"in_1","status":"paid","charge":"ch_1","payment_intent":"pi_1","amount_paid":999,"currency":"usd"}}`, "active", "ch_1", true, false, time.Unix(1782592000, 0).UTC()},
		{"intent fallback", `{"id":"sub_1","status":"active","latest_invoice":{"id":"in_1","status":"paid","payment_intent":"pi_2"}}`, "active", "pi_2", true, false, time.Time{}},
		{"invoice fallback", `{"id":"sub_1","status":"active","latest_invoice":{"id":"in_1","paid":true}}`, "active", "in_1", true, false, time.Time{}},
		{"pinned shape: item period, paid payment", `{"id":"sub_1","status":"ACTIVE","items":{"data":[{"current_period_end":1782592000,"price":{"id":"price_1"}}]},"latest_invoice":{"id":"in_1","status":"paid","payments":{"data":[{"status":"failed","payment":{"charge":"ch_failed"}},{"status":"paid","payment":{"charge":"ch_paid","payment_intent":"pi_paid"}}]}}}`, "active", "ch_paid", true, false, time.Unix(1782592000, 0).UTC()},
		{"open invoice after attempt", `{"id":"sub_1","status":"past_due","latest_invoice":{"id":"in_1","status":"open","attempt_count":2}}`, "past_due", "in_1", false, true, time.Time{}},
		{"open invoice not yet attempted", `{"id":"sub_1","status":"active","latest_invoice":{"id":"in_1","status":"open"}}`, "active", "in_1", false, false, time.Time{}},
		{"no invoice", `{"id":"sub_1","status":"canceled"}`, "canceled", "", false, false, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := parseStripeLivenessSubscription([]byte(tc.body))
			require.NoError(t, err)
			require.True(t, rec.Found)
			require.Equal(t, tc.status, rec.Status)
			require.Equal(t, tc.txn, rec.LatestInvoiceTransactionID)
			require.Equal(t, tc.paid, rec.LatestInvoicePaid)
			require.Equal(t, tc.failed, rec.LatestInvoiceCollectionFailed)
			require.Equal(t, tc.periodTo, rec.CurrentPeriodEnd)
		})
	}
	_, err := parseStripeLivenessSubscription([]byte(`{`))
	require.Error(t, err)
}

// 404 is Stripe's answer (remote absent), not a transport failure; any other
// error status must not be mistaken for absence.
func TestHTTPStripeLivenessProber(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/subscriptions/sub_42" || r.Header.Get("Authorization") != "Bearer sk_test_x" ||
			!slices.Equal([]string{"latest_invoice", "latest_invoice.payments"}, r.URL.Query()["expand[]"]) {
			t.Errorf("unexpected probe %s %s", r.Method, r.URL)
		}
		w.WriteHeader(int(status.Load()))
		if status.Load() == http.StatusOK {
			_, _ = w.Write([]byte(`{"id":"sub_42","status":"past_due"}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	t.Cleanup(srv.Close)
	prober := &HTTPStripeLivenessProber{SecretKey: "sk_test_x", BaseURL: srv.URL, HTTPClient: srv.Client()}
	ctx := context.Background()

	rec, err := prober.ProbeSubscription(ctx, "sub_42")
	require.NoError(t, err)
	require.True(t, rec.Found)
	require.Equal(t, "past_due", rec.Status)

	status.Store(http.StatusNotFound)
	rec, err = prober.ProbeSubscription(ctx, "sub_42")
	require.NoError(t, err)
	require.False(t, rec.Found)

	for _, code := range []int64{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		status.Store(code)
		_, err = prober.ProbeSubscription(ctx, "sub_42")
		require.Error(t, err, code)
	}

	_, err = prober.ProbeSubscription(ctx, " ")
	require.Error(t, err)
	_, err = (&HTTPStripeLivenessProber{BaseURL: srv.URL}).ProbeSubscription(ctx, "sub_42")
	require.Error(t, err, "no secret key")
}
