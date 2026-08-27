package nmi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		fmt.Fprint(w, "response=1&transactionid=t1&responsetext=APPROVED")
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.RunSale(context.Background(), SaleParams{
		CustomerVaultID:  "v1",
		Amount:           moneyutil.Cents(1999), // $19.99 — never 19_990_000
		Currency:         "USD",
		OrderID:          "o1",
		StoredCredential: testInitialOneTimeCredential(),
	})
	require.NoError(t, err)

	req, err := url.ParseQuery(string(body))
	require.NoError(t, err)
	assert.Equal(t, "19.99", req.Get("amount"), "sale wire amount must be the exact two-decimal cents rendering")
}

func TestRefund_WirePinsCentsAmount(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		fmt.Fprint(w, `{"id":"t2","response":"1","response_text":"APPROVED"}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.Refund(context.Background(), RefundParams{TransactionID: "t1", Amount: moneyutil.Cents(500)})
	require.NoError(t, err)

	var req map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, "5.00", string(req["amount"]), "refund wire amount must be the exact two-decimal cents rendering")

	// Amount 0 = full refund: no amount key on the wire.
	_, err = client.Refund(context.Background(), RefundParams{TransactionID: "t1"})
	require.NoError(t, err)
	fullBody := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal(body, &fullBody))
	_, hasAmount := fullBody["amount"]
	assert.False(t, hasAmount, "full refund must omit the amount key")
}

// The enrollment charge (type=sale + recurring=add_subscription) is real money:
// known CENTS in ⇒ the literal two-decimal wire string out, identical to the
// formatter the plan amount uses so the two can never disagree (#818).
func TestAddRecurringSubscription_WirePinsCentsAmount(t *testing.T) {
	cases := []struct {
		name  string
		cents moneyutil.Cents
		want  string
	}{
		{"whole dollars", 5000, "50.00"},
		{"dollars and cents", 1999, "19.99"},
		{"sub-dollar", 5, "0.05"},
		{"zero", 0, "0.00"},
		{"negative", -1999, "-19.99"},
		// The old float path formatted these via FormatFloat and lost the
		// trailing digit; the integer path renders every cent.
		{"whole cent above a dollar", 101, "1.01"},
		{"one cent", 1, "0.01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var form url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				form = r.Form
				fmt.Fprint(w, "response=1&responsetext=SUCCESS&subscription_id=sub1&transactionid=t3")
			}))
			defer server.Close()

			client := newTestClient(t, server.URL)
			_, err := client.AddRecurringSubscription(context.Background(), RecurringPaymentData{
				PlanID:           "plan1",
				CustomerVaultID:  "v1",
				Currency:         "USD",
				Amount:           tc.cents,
				StoredCredential: testInitialRecurringCredential(),
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, form.Get("amount"))
			// The enrollment charge and the plan amount MUST render identically.
			assert.Equal(t, centsToDollarString(tc.cents), form.Get("amount"))
		})
	}
}

// A sub-cent price can never reach the wire: the caller-side conversion errors
// rather than silently rounding a charge down (the #818 under-charge).
func TestSubCentMicrosNeverReachTheRecurringWire(t *testing.T) {
	for _, micros := range []int64{1_005_000, 999_999, 12_345, -1} {
		_, err := moneyutil.NativeToRailMinorExact("USD", micros)
		require.Error(t, err, "%d micros must not be representable in whole cents", micros)
	}
	cents, err := moneyutil.NativeToRailMinorExact("USD", 19_990_000)
	require.NoError(t, err)
	require.Equal(t, "19.99", centsToDollarString(moneyutil.Cents(cents)))
}
