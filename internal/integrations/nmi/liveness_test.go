package nmi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLivenessTestClient(t *testing.T, handler http.HandlerFunc) *NMIClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client, err := NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "test_key", WebhookSecret: "s",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL
	client.V5BaseURL = srv.URL
	return client
}

func TestProbeSalesByOrderID_SuccessfulSale(t *testing.T) {
	var gotStartDate, gotOrderID string
	client := newLivenessTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotStartDate = r.Form.Get("start_date")
		gotOrderID = r.Form.Get("order_id")
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
<nm_response>
  <transaction>
    <transaction_id>txn123</transaction_id>
    <order_id>order-abc</order_id>
    <action><action_type>sale</action_type><success>1</success><date>20260610120000</date></action>
  </transaction>
</nm_response>`))
	})

	since := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)
	res, err := client.ProbeSalesByOrderID("order-abc", since)
	require.NoError(t, err)
	assert.True(t, res.SuccessFound)
	assert.Equal(t, "txn123", res.SuccessTransactionID)
	assert.False(t, res.DeclineFound)
	assert.Equal(t, "20260609000000", gotStartDate, "since must be forwarded as the Query API start_date")
	assert.Equal(t, "order-abc", gotOrderID)
}

func TestProbeSalesByOrderID_DeclineCarriesEvidence(t *testing.T) {
	client := newLivenessTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
<nm_response>
  <transaction>
    <transaction_id>txn1</transaction_id>
    <order_id>order-abc</order_id>
    <action><action_type>sale</action_type><success>0</success><date>20260608120000</date><response_code>202</response_code><response_text>Insufficient funds</response_text></action>
  </transaction>
  <transaction>
    <transaction_id>txn2</transaction_id>
    <order_id>order-abc</order_id>
    <action><action_type>sale</action_type><success>0</success><date>20260610120000</date><response_code>252</response_code><response_text>Stolen card</response_text></action>
  </transaction>
</nm_response>`))
	})

	res, err := client.ProbeSalesByOrderID("order-abc", time.Time{})
	require.NoError(t, err)
	assert.False(t, res.SuccessFound)
	assert.True(t, res.DeclineFound)
	assert.Equal(t, 252, res.DeclineResponseCode, "the LATEST decline's evidence wins")
	assert.Equal(t, "Stolen card", res.DeclineReason)
}

func TestProbeSalesByOrderID_ClientSideDateFilter(t *testing.T) {
	// The signup sale shares the subscription's order reference but predates
	// the probed period; it must not count as the period's charge.
	client := newLivenessTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
<nm_response>
  <transaction>
    <transaction_id>signup</transaction_id>
    <order_id>order-abc</order_id>
    <action><action_type>sale</action_type><success>1</success><date>20260101120000</date></action>
  </transaction>
</nm_response>`))
	})

	res, err := client.ProbeSalesByOrderID("order-abc", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.False(t, res.SuccessFound, "sales before the period window must be filtered even if the server echoes them")
	assert.False(t, res.DeclineFound)
}

func TestProbeSalesByOrderID_MismatchedOrderIgnored(t *testing.T) {
	client := newLivenessTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
<nm_response>
  <transaction>
    <transaction_id>other</transaction_id>
    <order_id>different-order</order_id>
    <action><action_type>sale</action_type><success>1</success></action>
  </transaction>
</nm_response>`))
	})

	res, err := client.ProbeSalesByOrderID("order-abc", time.Time{})
	require.NoError(t, err)
	assert.False(t, res.SuccessFound)
}

func TestProbeSalesByOrderID_ErrorResponse(t *testing.T) {
	client := newLivenessTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response><error_response>Invalid security key</error_response></nm_response>`))
	})
	_, err := client.ProbeSalesByOrderID("order-abc", time.Time{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Invalid security key")
}

func TestFindSuccessfulSaleByOrderID_NoDateBound(t *testing.T) {
	// The manual-rebill verifier's question: ANY successful sale for the
	// order reference counts, whenever it happened.
	client := newLivenessTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		assert.Empty(t, r.Form.Get("start_date"))
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
<nm_response>
  <transaction>
    <transaction_id>txn-old</transaction_id>
    <order_id>rebill-x-1</order_id>
    <action><action_type>sale</action_type><success>1</success><date>20250101120000</date></action>
  </transaction>
</nm_response>`))
	})

	txn, found, err := client.FindSuccessfulSaleByOrderID("rebill-x-1")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "txn-old", txn)
}

func TestGetRecurringLiveness_FoundWithNextCharge(t *testing.T) {
	var gotPath string
	client := newLivenessTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"object":"subscription","id":"sub-77","delayed_condition":"active","next_billing_date":"2026-07-01"}`))
	})

	liveness, err := client.GetRecurringLiveness("sub-77")
	require.NoError(t, err)
	assert.True(t, liveness.Found)
	assert.Equal(t, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), liveness.NextChargeDate)
	assert.Equal(t, "/subscriptions/sub-77", gotPath)
}

func TestGetRecurringLiveness_ISO8601NextBilling(t *testing.T) {
	client := newLivenessTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"subscription","id":"sub-77","next_billing_date":"2026-07-01T00:00:00.000Z"}`))
	})
	liveness, err := client.GetRecurringLiveness("sub-77")
	require.NoError(t, err)
	assert.True(t, liveness.Found)
	assert.Equal(t, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), liveness.NextChargeDate)
}

func TestGetRecurringLiveness_DeletionTombstoneIsAbsent(t *testing.T) {
	// Live-verified 2026-07-01: the v5 GET keeps answering 200 for deleted
	// subscriptions with delayed_condition=inactive. That tombstone must read
	// as GONE, matching the classic report's absence-is-terminal semantics.
	client := newLivenessTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"subscription","id":"sub-77","delayed_condition":"inactive","next_billing_date":"2026-07-29"}`))
	})
	liveness, err := client.GetRecurringLiveness("sub-77")
	require.NoError(t, err)
	assert.False(t, liveness.Found)
}

func TestGetRecurringLiveness_Absent(t *testing.T) {
	client := newLivenessTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"notFound","error_code":"E_NOT_FOUND","message":"subscription not found"}`))
	})
	liveness, err := client.GetRecurringLiveness("sub-gone")
	require.NoError(t, err)
	assert.False(t, liveness.Found, "404 from the v5 subscription GET IS terminal at NMI")
}

func TestGetRecurringLiveness_ErrorResponse(t *testing.T) {
	client := newLivenessTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"internalError","error_code":"E_INTERNAL","message":"boom"}`))
	})
	_, err := client.GetRecurringLiveness("sub-77")
	require.Error(t, err)
}
