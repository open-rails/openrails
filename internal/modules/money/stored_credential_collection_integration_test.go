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

// TestChargeOutstanding_StoredCredentialMITAndBackfill is the #297 collection
// proof over the charge seam (ChargeOutstanding → ScopedCharger →
// NMICollectionAdapter → nmidirect.Charger → classic Direct Post):
//
//  1. a LEGACY instrument (no stored-credential reference) collects as a
//     reference-less unscheduled MIT (initiated_by=merchant +
//     stored_credential_indicator=used, no initial_transaction_id) — the
//     deliberate fail-open policy — and the success BACK-FILLS the anchor;
//  2. the next collection on the SAME instrument replays the persisted anchor
//     as initial_transaction_id.
func TestChargeOutstanding_StoredCredentialMITAndBackfill(t *testing.T) {
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

	// Legacy instrument: seeded with NO stored-credential references.
	pm := seedPaymentMethodWithRailCustomerRef(t, pool, ctx, payer, string(models.RailNMI), "vault-297-"+uuid.NewString()[:8])

	charger := money.NewScopedCharger(dbi, money.NewNMICollectionAdapters(map[string]*nmi.NMIClient{
		string(models.RailNMI): client,
	}))

	openAndCollect := func(sourceID string) {
		t.Helper()
		_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
			BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
		})
		require.NoError(t, err)
		require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 2_000_000))
		_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", sourceID, 150*10_000)
		require.NoError(t, err)
		_, err = svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		require.NoError(t, err)
		n, err := svc.ChargeOutstanding(ctx, charger, 0)
		require.NoError(t, err)
		require.Equal(t, 1, n)
	}

	refOf := func() string {
		t.Helper()
		var ref string
		require.NoError(t, pool.QueryRow(ctx,
			"SELECT stored_credential_unscheduled_ref FROM openrails.payment_methods WHERE id = $1", pm).Scan(&ref))
		return ref
	}

	// Leg 1: legacy reference-less MIT + backfill.
	require.Empty(t, refOf(), "legacy instrument starts unanchored")
	openAndCollect("297-leg1-" + uuid.NewString()[:8])
	form := fake.form(t, 0)
	assert.Equal(t, "merchant", form.Get("initiated_by"))
	assert.Equal(t, "used", form.Get("stored_credential_indicator"))
	_, hasInitial := form["initial_transaction_id"]
	assert.False(t, hasInitial, "legacy instrument charges reference-less (fail-open policy)")
	assert.Equal(t, "txn-297-1", refOf(), "success back-fills the unscheduled anchor write-once")

	// Leg 2: the anchored instrument replays the reference.
	openAndCollect("297-leg2-" + uuid.NewString()[:8])
	form = fake.form(t, 1)
	assert.Equal(t, "merchant", form.Get("initiated_by"))
	assert.Equal(t, "used", form.Get("stored_credential_indicator"))
	assert.Equal(t, "txn-297-1", form.Get("initial_transaction_id"), "MIT carries the persisted anchor")
	assert.Equal(t, "txn-297-1", refOf(), "anchor is write-once — never overwritten by later charges")
}
