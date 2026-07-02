package nmi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wire-pinning tests for the typed NMI money boundaries (#671): a known typed
// amount in ⇒ the LITERAL wire value out.

func TestRunSale_WirePinsCentsAmount(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		fmt.Fprint(w, `{"id":"t1","response":"1","response_text":"APPROVED"}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.RunSale(SaleParams{
		CustomerVaultID: "v1",
		Amount:          moneyutil.Cents(1999), // $19.99 — never 19_990_000
		Currency:        "USD",
		OrderID:         "o1",
	})
	require.NoError(t, err)

	var req map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, "19.99", string(req["amount"]), "sale wire amount must be the exact two-decimal cents rendering")
}

func TestRefund_WirePinsCentsAmount(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		fmt.Fprint(w, `{"id":"t2","response":"1","response_text":"APPROVED"}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.Refund(RefundParams{TransactionID: "t1", Amount: moneyutil.Cents(500)})
	require.NoError(t, err)

	var req map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, "5.00", string(req["amount"]), "refund wire amount must be the exact two-decimal cents rendering")

	// Amount 0 = full refund: no amount key on the wire.
	_, err = client.Refund(RefundParams{TransactionID: "t1"})
	require.NoError(t, err)
	fullBody := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal(body, &fullBody))
	_, hasAmount := fullBody["amount"]
	assert.False(t, hasAmount, "full refund must omit the amount key")
}

func TestAddRecurringSubscription_WirePinsDollarsAmount(t *testing.T) {
	var form string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form = string(b)
		fmt.Fprint(w, "response=1&responsetext=SUCCESS&subscription_id=sub1&transactionid=t3")
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.AddRecurringSubscription(RecurringPaymentData{
		PlanID:          "plan1",
		CustomerVaultID: "v1",
		Currency:        "USD",
		Amount:          moneyutil.MajorUnits(19.99), // decimal DOLLARS
	})
	require.NoError(t, err)
	assert.Contains(t, form, "amount=19.99", "recurring first-charge amount must be the two-decimal dollars rendering")
}
