package nmidirect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/stretchr/testify/require"
)

func recurringFixture(t *testing.T, handler http.HandlerFunc) *Charger {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := nmi.NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "account-key"}, true)
	require.NoError(t, err)
	client.DirectPostURL, client.V5BaseURL, client.QueryURL = server.URL+"/sale", server.URL, server.URL+"/query"
	return New(client)
}

func TestRecurringNativeWireAndNoProviderSchedule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		context  charge.Context
		initial  bool
		captured string
	}{
		{"initial CIT", charge.InitialRecurring(), true, "txn"},
		{"anchored CIT", charge.RecurringReuse("anchor"), true, ""},
		{"renewal MIT", charge.RecurringMIT("anchor"), false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire url.Values
			reads, sales := 0, 0
			c := recurringFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/customers/v1":
					reads++
					require.Equal(t, "account-key", r.Header.Get("Authorization"))
					fmt.Fprint(w, `{"object":"customer","id":"v1","billing":[{"id":"b1"}]}`)
				case "/sale":
					sales++
					require.NoError(t, r.ParseForm())
					wire = r.PostForm
					fmt.Fprint(w, "response=1&response_code=100&transactionid=txn")
				default:
					t.Errorf("unexpected provider path %s", r.URL.Path)
				}
			})
			var result charge.Result
			var refusal *nmi.CustomerVaultError
			var err error
			if tc.initial {
				result, refusal, err = c.ChargeInitialRecurring(context.Background(), baseRequest(tc.context))
			} else {
				result, refusal, err = c.ChargeRecurringMIT(context.Background(), baseRequest(tc.context))
			}
			require.NoError(t, err)
			require.Nil(t, refusal)
			require.Equal(t, 1, reads)
			require.Equal(t, 1, sales)
			require.Equal(t, "txn", result.TransactionID)
			require.Equal(t, tc.captured, result.CapturedRef)
			require.Equal(t, "sale", wire.Get("type"))
			require.Empty(t, wire.Get("recurring"))
			require.Empty(t, wire.Get("subscription_id"))
			require.Empty(t, wire.Get("plan_id"))
			require.Equal(t, "recurring", wire.Get("billing_method"))
			require.Equal(t, string(tc.context.Initiator), wire.Get("initiated_by"))
			indicator := "used"
			if tc.context.FirstUse {
				indicator = "stored"
			}
			require.Equal(t, indicator, wire.Get("stored_credential_indicator"))
			require.Equal(t, tc.context.PriorRef, wire.Get("initial_transaction_id"))
			require.Equal(t, "v1", wire.Get("customer_vault_id"))
			require.Equal(t, "b1", wire.Get("billing_id"))
			require.Equal(t, "account-key", wire.Get("security_key"))
			require.Equal(t, "5.00", wire.Get("amount"))
			require.Equal(t, "USD", wire.Get("currency"))
			require.Equal(t, "order-1", wire.Get("orderid"))
		})
	}
}

func TestRecurringNativeInvalidTermsNeverReachProvider(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*charge.Request, *Charger)
	}{
		{"missing anchor", func(r *charge.Request, c *Charger) { r.Context = charge.RecurringMIT("") }},
		{"unscheduled", func(r *charge.Request, c *Charger) { r.Context = charge.UnscheduledMIT("anchor") }},
		{"wrong rail", func(r *charge.Request, c *Charger) { r.Instrument.Rail = "stripe" }},
		{"empty billing", func(r *charge.Request, c *Charger) { r.Instrument.MethodRef = "" }},
		{"whitespace vault", func(r *charge.Request, c *Charger) { r.Instrument.CustomerRef = " v1" }},
		{"unknown currency", func(r *charge.Request, c *Charger) { r.Currency = "??" }},
		{"zero amount", func(r *charge.Request, c *Charger) { r.AmountMinor = 0 }},
		{"missing operation", func(r *charge.Request, c *Charger) { r.OrderRef = "" }},
		{"changed credentials", func(r *charge.Request, c *Charger) { c.Client.SecurityKey = "another-account" }},
		{"read only", func(r *charge.Request, c *Charger) { c.Client.ReadOnly = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := recurringFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid recurring request reached provider") })
			req := baseRequest(charge.RecurringMIT("anchor"))
			tc.change(&req, c)
			_, refusal, err := c.ChargeRecurringMIT(context.Background(), req)
			require.ErrorIs(t, err, charge.ErrNotDispatched)
			require.Nil(t, refusal)
		})
	}
}

func TestRecurringNativeVaultMismatchNeverSendsMoney(t *testing.T) {
	for _, billing := range []string{`[]`, `[{"id":"other"}]`, `[{"id":"b1"},{"id":"b2"}]`} {
		t.Run(billing, func(t *testing.T) {
			reads := 0
			c := recurringFixture(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "GET", r.Method)
				require.Equal(t, "/customers/v1", r.URL.Path)
				reads++
				fmt.Fprintf(w, `{"object":"customer","id":"v1","billing":%s}`, billing)
			})
			_, _, err := c.ChargeRecurringMIT(context.Background(), baseRequest(charge.RecurringMIT("anchor")))
			require.ErrorIs(t, err, charge.ErrNotDispatched)
			require.Equal(t, 1, reads)
		})
	}
}

func TestRecurringNativeRefusalAndUnknownAreDistinctAndNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		hard           bool
	}{
		{"decline", "response=2&response_code=202&responsetext=secret", true},
		{"communication", "response=3&response_code=420", false},
		{"duplicate", "response=3&response_code=430", false},
		{"malformed", "bad payload", false},
		{"unqualified gateway error", "response=3&response_code=400", false},
		{"contradictory decline", "response=3&response_code=202", false},
		{"approval missing id", "response=1&response_code=100", false},
		{"lost response", "disconnect", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sales atomic.Int32
			c := recurringFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					fmt.Fprint(w, `{"object":"customer","id":"v1","billing":[{"id":"b1"}]}`)
					return
				}
				sales.Add(1)
				if tc.response == "disconnect" {
					conn, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					require.NoError(t, conn.Close())
					return
				}
				fmt.Fprint(w, tc.response)
			})
			result, refusal, err := c.ChargeRecurringMIT(context.Background(), baseRequest(charge.RecurringMIT("anchor")))
			require.EqualValues(t, 1, sales.Load())
			if tc.hard {
				require.NoError(t, err)
				require.NotNil(t, refusal)
				require.True(t, result.Declined)
				require.Equal(t, 202, refusal.ResponseCode)
				require.Equal(t, "response=2&response_code=202", refusal.RawResponse)
				require.False(t, strings.Contains(refusal.Error(), "secret"))
			} else {
				require.Error(t, err)
				require.NotErrorIs(t, err, charge.ErrNotDispatched)
				require.Nil(t, refusal)
				require.False(t, result.Declined)
			}
		})
	}
}

func TestNativeProviderAndEngineShareVaultWithoutSharingSchedule(t *testing.T) {
	var forms []url.Values
	c := recurringFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"object":"customer","id":"v1","billing":[{"id":"b1"}]}`)
			return
		}
		require.NoError(t, r.ParseForm())
		forms = append(forms, r.PostForm)
		fmt.Fprint(w, "response=1&response_code=100&transactionid=txn&subscription_id=provider-sub")
	})
	// Existing provider enrollment retains its native scheduling contract on the
	// exact same vault/billing entry used by a separate engine agreement.
	_, err := c.Client.AddRecurringSubscription(t.Context(), nmi.RecurringPaymentData{CustomerVaultID: "v1", BillingID: "b1", PlanID: "provider-plan", Currency: "USD", Amount: 500, OrderID: "provider-order", StoredCredential: StoredCredentialFor(charge.InitialRecurring())})
	require.NoError(t, err)
	_, _, err = c.ChargeInitialRecurring(t.Context(), baseRequest(charge.InitialRecurring()))
	require.NoError(t, err)
	_, _, err = c.ChargeRecurringMIT(t.Context(), baseRequest(charge.RecurringMIT("engine-anchor")))
	require.NoError(t, err)
	require.Len(t, forms, 3)
	require.Equal(t, "add_subscription", forms[0].Get("recurring"))
	require.Equal(t, "provider-plan", forms[0].Get("plan_id"))
	for _, f := range forms[1:] {
		require.Equal(t, "sale", f.Get("type"))
		require.Empty(t, f.Get("recurring"))
		require.Empty(t, f.Get("plan_id"))
		require.Empty(t, f.Get("subscription_id"))
	}
	for _, f := range forms {
		require.Equal(t, "v1", f.Get("customer_vault_id"))
		require.Equal(t, "b1", f.Get("billing_id"))
	}
}
