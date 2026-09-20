//go:build integration

package money_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
)

// fakeClassicNMIGateway records every classic Direct Post sale form and
// approves with a scripted transaction id per call.
type fakeClassicNMIGateway struct {
	mu    sync.Mutex
	forms []url.Values
	txn   func(call int) string
}

func (f *fakeClassicNMIGateway) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet {
			for i, form := range f.forms {
				txn := f.txn(i + 1)
				if r.URL.Path == "/payments/"+txn {
					_ = json.NewEncoder(w).Encode(map[string]any{"id": txn, "response": "1", "currency": form.Get("currency"), "customer_vault_id": form.Get("customer_vault_id"), "actions": []map[string]any{{"type": "sale", "success": true, "amount": form.Get("amount")}}})
					return
				}
			}
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("type") == "sale" {
			f.forms = append(f.forms, r.Form)
			fmt.Fprintf(w, "response=1&responsetext=SUCCESS&authcode=OK&transactionid=%s&response_code=100", f.txn(len(f.forms)))
			return
		}
		fmt.Fprint(w, "<nm_response>")
		for i, form := range f.forms {
			if form.Get("orderid") == r.Form.Get("order_id") {
				fmt.Fprintf(w, `<transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction>`, f.txn(i+1), form.Get("orderid"))
			}
		}
		fmt.Fprint(w, "</nm_response>")
	}
}

func (f *fakeClassicNMIGateway) form(t *testing.T, i int) url.Values {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Greater(t, len(f.forms), i)
	return f.forms[i]
}

// Pre-v1 historical fallbacks are unsupported. An unscheduled collection needs
// its explicitly approved agreement anchor; neither an unscoped initial ID nor
// a successful earlier merchant-initiated charge can stand in for that consent.
func TestChargeOutstanding_StoredCredentialMITRequiresScopedAnchor(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, initial := range []string{"", "unscoped-initial"} {
			t.Run(fmt.Sprintf("scoped=%v/initial=%v", scoped, initial != ""), func(t *testing.T) {
				svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
				cleanupCollection(t, pool, ctx, payer)
				fake := &fakeClassicNMIGateway{txn: func(call int) string { return fmt.Sprintf("txn-297-%d", call) }}
				server := httptest.NewServer(fake.handler())
				t.Cleanup(server.Close)
				msvc := merchantsServiceForTest(t, dbi)
				seedPSPSecrets(t, dbi, msvc, "nmi", "anchor-"+uuid.NewString(), map[string]string{"security_key": "test-key"})
				pm := seedPaymentMethodWithRailCustomerRef(t, pool, ctx, payer, string(models.RailNMI), "vault-297-"+uuid.NewString())
				anchor := ""
				if scoped {
					anchor = "approved-unscheduled-cit"
				}
				_, err := pool.Exec(ctx, `UPDATE openrails.payment_methods SET stored_credential_unscheduled_ref=$2,initial_transaction_id=$3 WHERE id=$1`, pm, anchor, initial)
				require.NoError(t, err)
				invoice := seedArrearsInvoice(t, svc, ctx, payer, pm)
				plane := &money.MerchantCollectionAdapterBuilder{Config: storeCollectionTestConfig(), DB: dbi, MerchantsFn: func() *merchants.Service { return msvc }, Endpoints: money.CollectionEndpoints{NMIDirectPostURL: server.URL, NMIQueryURL: server.URL, NMIV5BaseURL: server.URL}}
				charger := money.NewScopedCharger(dbi, nil)
				charger.SetAdapterResolver(plane)
				n, err := svc.ChargeOutstanding(ctx, collectionRunner(dbi, charger, plane), 0)
				require.NoError(t, err)
				if !scoped {
					require.Zero(t, n)
					fake.mu.Lock()
					require.Empty(t, fake.forms, "no provider sale without an approved agreement reference")
					fake.mu.Unlock()
					op := latestCollectionIntent(t, pool, ctx, invoice)
					require.NotNil(t, op.LastFailureReason)
					require.True(t, strings.Contains(*op.LastFailureReason, "unscheduled credential reference"))
					return
				}
				require.Equal(t, 1, n)
				form := fake.form(t, 0)
				require.Equal(t, "merchant", form.Get("initiated_by"))
				require.Equal(t, "used", form.Get("stored_credential_indicator"))
				require.Equal(t, anchor, form.Get("initial_transaction_id"))
				var retained string
				require.NoError(t, pool.QueryRow(ctx, "SELECT stored_credential_unscheduled_ref FROM openrails.payment_methods WHERE id=$1", pm).Scan(&retained))
				require.Equal(t, anchor, retained, "MIT never replaces the approved CIT anchor")
			})
		}
	}
}
