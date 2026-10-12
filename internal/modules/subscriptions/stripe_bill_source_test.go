package subscriptions

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripeBillDraftDoesNotFailSubscriptionProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/subscriptions/sub_1", r.URL.Path, "a draft has no finalized bill to fetch")
		fmt.Fprint(w, `{"id":"sub_1","status":"active","customer":"cus_1","latest_invoice":{"id":"in_draft","status":"draft","attempt_count":0,"amount_due":999,"currency":"usd"}}`)
	}))
	defer srv.Close()
	p := &HTTPStripeLivenessProber{SecretKey: "sk_test_fixture", BaseURL: srv.URL, HTTPClient: srv.Client()}
	rec, err := p.ProbeSubscription(t.Context(), "sub_1")
	require.NoError(t, err)
	require.True(t, rec.Found)
	require.Equal(t, "active", rec.Status)
	require.Nil(t, rec.LatestBill)
	require.False(t, rec.LatestInvoicePaid)
	require.False(t, rec.LatestInvoiceCollectionFailed)
}

func TestStripeBillReadsEachActualPayment(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		requests = append(requests, r.URL.Path)
		switch r.URL.Path {
		case "/v1/invoices/in_1":
			fmt.Fprint(w, `{"id":"in_1","status":"paid","customer":"cus_1","parent":{"subscription_details":{"subscription":"sub_1"}},"currency":"usd","total":2000,"amount_due":2000,"amount_paid":2000,"amount_remaining":0,"lines":{"data":[],"has_more":false},"payments":{"data":[],"has_more":true}}`)
		case "/v1/invoice_payments":
			require.Equal(t, "in_1", r.URL.Query().Get("invoice"))
			if r.URL.Query().Get("starting_after") == "" {
				fmt.Fprint(w, `{"has_more":true,"data":[{"id":"inpay_1","invoice":"in_1","status":"paid","currency":"usd","amount_paid":800,"payment":{"type":"payment_intent","payment_intent":"pi_1"}}]}`)
			} else {
				require.Equal(t, "inpay_1", r.URL.Query().Get("starting_after"))
				fmt.Fprint(w, `{"has_more":false,"data":[{"id":"inpay_2","invoice":"in_1","status":"paid","currency":"usd","amount_paid":1200,"payment":{"type":"charge","charge":"ch_2"}}]}`)
			}
		case "/v1/payment_intents/pi_1":
			fmt.Fprint(w, `{"id":"pi_1","status":"succeeded","currency":"usd","amount_received":800,"latest_charge":"ch_1"}`)
		case "/v1/charges/ch_1":
			fmt.Fprint(w, `{"id":"ch_1","status":"succeeded","paid":true,"captured":true,"currency":"usd","amount_captured":800,"payment_intent":"pi_1"}`)
		case "/v1/charges/ch_2":
			fmt.Fprint(w, `{"id":"ch_2","status":"succeeded","paid":true,"captured":true,"currency":"usd","amount_captured":1200}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	p := &HTTPStripeLivenessProber{SecretKey: "sk_test_fixture", BaseURL: srv.URL, HTTPClient: srv.Client()}
	bill, err := p.ReadBill(context.Background(), "in_1")
	require.NoError(t, err)
	require.Equal(t, "sub_1", bill.SubscriptionID)
	require.Len(t, bill.Payments, 2)
	require.Equal(t, int64(800), bill.Payments[0].Amount)
	require.Equal(t, "ch_1", bill.Payments[0].ChargeID)
	require.Equal(t, int64(1200), bill.Payments[1].Amount)
	require.Equal(t, "ch_2", bill.Payments[1].ChargeID)
	require.Len(t, requests, 6)
}

func TestStripePaidInvoiceDoesNotInventMoney(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		noncash      int64
	}{
		{"out of band", `"total":2000,"amount_due":2000,"amount_paid":2000,"amount_paid_off_stripe":2000`, 2000},
		{"zero bill", `"total":0,"amount_due":0,"amount_paid":0`, 0},
		{"customer credit", `"total":2000,"amount_due":0,"amount_paid":0,"starting_balance":-2000`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/v1/invoices/in_1", r.URL.Path)
				fmt.Fprintf(w, `{"id":"in_1","status":"paid","currency":"usd",%s,"lines":{"data":[]},"payments":{"data":[]}}`, tc.fields)
			}))
			t.Cleanup(srv.Close)
			bill, err := (&HTTPStripeLivenessProber{SecretKey: "sk_test_fixture", BaseURL: srv.URL, HTTPClient: srv.Client()}).ReadBill(context.Background(), "in_1")
			require.NoError(t, err)
			require.Empty(t, bill.Payments)
			require.Equal(t, tc.noncash, bill.AmountOffStripe)
		})
	}
}

func TestStripeInvoiceAllocationDoesNotCounterfeitWholeCharge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/payment_intents/pi_shared" {
			fmt.Fprint(w, `{"id":"pi_shared","status":"succeeded","currency":"usd","amount_received":2000,"latest_charge":"ch_shared"}`)
			return
		}
		t.Errorf("unexpected request %s", r.URL)
	}))
	t.Cleanup(srv.Close)
	p := &HTTPStripeLivenessProber{SecretKey: "sk_test_fixture", BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := p.readBillPayment(context.Background(), "in_1", []byte(`{"id":"inpay_1","invoice":"in_1","status":"paid","currency":"usd","amount_paid":800,"payment":{"type":"payment_intent","payment_intent":"pi_shared"}}`))
	require.ErrorContains(t, err, "cannot be represented as one complete money movement")
}

func TestStripeBillHistoryRejectsBrokenPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"has_more":true,"data":[]}`)
	}))
	t.Cleanup(srv.Close)
	p := &HTTPStripeLivenessProber{SecretKey: "sk_test_fixture", BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := p.VisitSubscriptionBills(context.Background(), "sub_1", func(StripeBill) error { t.Fatal("empty page produced a bill"); return nil })
	require.ErrorContains(t, err, "incomplete")
}
