//go:build integration

package money_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
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
		_ = r.ParseForm()
		if r.Form.Get("type") != "sale" {
			fmt.Fprint(w, "response=1")
			return
		}
		f.mu.Lock()
		f.forms = append(f.forms, r.Form)
		call := len(f.forms)
		f.mu.Unlock()
		fmt.Fprintf(w, "response=1&responsetext=SUCCESS&authcode=OK&transactionid=%s&response_code=100", f.txn(call))
	}
}

func (f *fakeClassicNMIGateway) form(t *testing.T, i int) url.Values {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Greater(t, len(f.forms), i)
	return f.forms[i]
}

// TestChargeOutstanding_StoredCredentialMITFallbacks is the #297 collection
// proof over the charge seam (ChargeOutstanding → ScopedCharger →
// NMICollectionAdapter → nmidirect.Charger → classic Direct Post):
//
//  1. a missing agreement-scoped anchor uses the legacy initial transaction;
//  2. a fully unanchored historical row still sends merchant+used best effort;
//  3. an agreement-scoped anchor always wins and rides initial_transaction_id.
func TestChargeOutstanding_StoredCredentialMITFallbacks(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	cleanupInvoiceRows(t, pool, ctx, payer)

	fake := &fakeClassicNMIGateway{txn: func(call int) string { return fmt.Sprintf("txn-297-%d", call) }}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)

	client, err := nmi.NewClient(string(models.RailNMI), &config.NMIProviderSettings{SecurityKey: "test-key"}, true)
	require.NoError(t, err)
	client.DirectPostURL = server.URL
	client.QueryURL = server.URL
	client.V5BaseURL = server.URL

	// Instrument starts with no stored-credential references.
	pm := seedPaymentMethodWithRailCustomerRef(t, pool, ctx, payer, string(models.RailNMI), "vault-297-"+uuid.NewString()[:8])
	_, err = pool.Exec(ctx,
		"UPDATE openrails.payment_methods SET stored_credential_unscheduled_ref = '', initial_transaction_id = 'legacy-initial-297' WHERE id = $1", pm)
	require.NoError(t, err)

	charger := money.NewScopedCharger(dbi, money.NewNMICollectionAdapters(map[string]*nmi.NMIClient{
		string(models.RailNMI): client,
	}))

	refOf := func() string {
		t.Helper()
		var ref string
		require.NoError(t, pool.QueryRow(ctx,
			"SELECT stored_credential_unscheduled_ref FROM openrails.payment_methods WHERE id = $1", pm).Scan(&ref))
		return ref
	}

	_, err = svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 2_000_000))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "297-anchor-"+uuid.NewString()[:8], 150*10_000)
	require.NoError(t, err)
	_, err = svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	// Leg 1: no scoped anchor uses the older vault-creation transaction ID.
	require.Empty(t, refOf(), "instrument starts without an agreement-scoped anchor")
	n, err := svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	legacyForm := fake.form(t, 0)
	assert.Equal(t, "merchant", legacyForm.Get("initiated_by"))
	assert.Equal(t, "used", legacyForm.Get("stored_credential_indicator"))
	assert.Equal(t, "legacy-initial-297", legacyForm.Get("initial_transaction_id"))
	assert.Empty(t, refOf())

	// Leg 2: when neither reference can be recovered, do the explicit
	// availability-first MIT and omit only initial_transaction_id.
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "297-unanchored-"+uuid.NewString()[:8], 150*10_000)
	require.NoError(t, err)
	_, err = svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-2*time.Hour), time.Now().Add(2*time.Hour))
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		"UPDATE openrails.payment_methods SET initial_transaction_id = '' WHERE id = $1", pm)
	require.NoError(t, err)
	n, err = svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	unanchoredForm := fake.form(t, 1)
	assert.Equal(t, "merchant", unanchoredForm.Get("initiated_by"))
	assert.Equal(t, "used", unanchoredForm.Get("stored_credential_indicator"))
	assert.NotContains(t, unanchoredForm, "initial_transaction_id")
	assert.Empty(t, refOf(), "fallback MIT is not promoted as an initial CIT")

	// Leg 3: a customer-present transaction establishes the scoped anchor; collection
	// replays it without replacing it with an MIT transaction.
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "297-anchored-"+uuid.NewString()[:8], 150*10_000)
	require.NoError(t, err)
	_, err = svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-3*time.Hour), time.Now().Add(3*time.Hour))
	require.NoError(t, err)
	const anchor = "approved-cit-297"
	_, err = pool.Exec(ctx,
		"UPDATE openrails.payment_methods SET stored_credential_unscheduled_ref = $2 WHERE id = $1",
		pm, anchor)
	require.NoError(t, err)
	n, err = svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	form := fake.form(t, 2)
	assert.Equal(t, "merchant", form.Get("initiated_by"))
	assert.Equal(t, "used", form.Get("stored_credential_indicator"))
	assert.Equal(t, anchor, form.Get("initial_transaction_id"), "MIT carries the approved CIT anchor")
	assert.Equal(t, anchor, refOf(), "MIT never overwrites the CIT anchor")
}
