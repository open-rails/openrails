package subscriptions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// #671 TEST WALL — Stripe outbound wire pinning: a known Cents amount (already
// converted from micros upstream via MicrosToCentsExact / NativeToRailMinor,
// both pinned in their own suites) must appear as the EXACT integer string in
// the outbound form body. All requests flow through the stripeapi choke point
// to a captured httptest server.

func wirePinStripeRails() config.RailMerchantAccountSet {
	return config.RailMerchantAccountSet{
		"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_wirepin"}},
	}
}

// TestStripeCollectInvoice_WirePinsCentsAmount: an arrears/topup charge of
// 19_990_000 micros reaches this adapter as Cents(1999) and MUST hit the wire
// as amount=1999 on POST /v1/invoiceitems — never 19990000 (micros) and never
// 19 (major units).
func TestStripeCollectInvoice_WirePinsCentsAmount(t *testing.T) {
	var itemForm url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/invoiceitems", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		itemForm = r.PostForm
		_, _ = w.Write([]byte(`{"id":"ii_1"}`))
	})
	mux.HandleFunc("POST /v1/invoices", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"in_1","status":"open"}`))
	})
	mux.HandleFunc("POST /v1/invoices/in_1/finalize", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"in_1","status":"paid","amount_paid":1999,"payment_intent":"pi_1","charge":"ch_1"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	svc := &StripeService{Config: &config.Config{}, Rails: wirePinStripeRails()}
	svc.SetBaseURLForTest(srv.URL)

	result, err := svc.CollectInvoice(context.Background(), StripeInvoiceCollectionParams{
		CustomerID:      "cus_1",
		PaymentMethodID: "pm_1",
		AmountCents:     1999, // ⇐ 19_990_000 micros upstream
		Currency:        "USD",
		IdempotencyKey:  "idem-1",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"1999"}, itemForm["amount"], "invoice item amount must be the literal cents value")
	assert.Equal(t, []string{"usd"}, itemForm["currency"], "currency must be lowercased on the wire")
	assert.Equal(t, "ch_1", result.ChargeID)
}

// TestStripeCollectInvoice_RejectsUnderpaidInvoice: the paid check compares
// cents to cents — an invoice settled below the requested cents errors.
func TestStripeCollectInvoice_RejectsUnderpaidInvoice(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/invoiceitems", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ii_1"}`))
	})
	mux.HandleFunc("POST /v1/invoices", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"in_1","status":"open"}`))
	})
	mux.HandleFunc("POST /v1/invoices/in_1/finalize", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"in_1","status":"paid","amount_paid":1998,"charge":"ch_1"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	svc := &StripeService{Config: &config.Config{}, Rails: wirePinStripeRails()}
	svc.SetBaseURLForTest(srv.URL)

	_, err := svc.CollectInvoice(context.Background(), StripeInvoiceCollectionParams{
		CustomerID:      "cus_1",
		PaymentMethodID: "pm_1",
		AmountCents:     1999,
		Currency:        "usd",
		IdempotencyKey:  "idem-1",
	})
	require.ErrorContains(t, err, "paid only 1998 of 1999")
}

// TestStripeCreateRefund_WirePinsCentsAmount: RefundPayload.AmountCents (cents,
// converted from admin-request micros at prepare time) must hit POST /v1/refunds
// as amount=<literal cents>; Amount 0 means full refund — the amount key must be
// OMITTED, not sent as "0".
func TestStripeCreateRefund_WirePinsCentsAmount(t *testing.T) {
	cases := []struct {
		name       string
		amount     int64 // cents; kept raw so the wire expectation stays literal
		wantAmount []string
	}{
		{"partial refund pins literal cents", 500, []string{"500"}}, // ⇐ 5_000_000 micros upstream
		{"19.99 refund", 1999, []string{"1999"}},
		{"zero means full refund: amount omitted", 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var refundForm url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/v1/refunds", r.URL.Path)
				require.NoError(t, r.ParseForm())
				refundForm = r.PostForm
				_, _ = w.Write([]byte(`{"id":"re_1","status":"succeeded","charge":"ch_1"}`))
			}))
			t.Cleanup(srv.Close)

			svc := &StripeRefundService{Config: &config.Config{}, Rails: wirePinStripeRails(), BaseURL: srv.URL}
			result, err := svc.CreateRefund(context.Background(), RefundParams{
				ChargeID:       "ch_1",
				Amount:         moneyutil.Cents(tc.amount),
				IdempotencyKey: "idem-refund",
			})
			require.NoError(t, err)
			assert.Equal(t, tc.wantAmount, refundForm["amount"])
			assert.Equal(t, []string{"ch_1"}, refundForm["charge"])
			assert.Equal(t, "re_1", result.ID)
		})
	}
}
