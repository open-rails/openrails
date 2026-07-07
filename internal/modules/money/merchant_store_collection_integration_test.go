//go:build integration

package money_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

// #725: the arrears collection path must arm rail credentials PER MERCHANT
// from the merchant-secrets store at charge time — store wins, boot-plane
// rails only when the merchant declares no account. These tests run the REAL
// ChargeOutstanding path against fake provider HTTP servers with credentials
// that exist ONLY in the store (nothing in the boot plane), plus the boot
// fallback and the fail-closed (declared-but-secretless) contract.

func storeCollectionTestConfig() *config.Config {
	// TestMode=sandbox → provider environment "test", matching
	// merchantsServiceForTest / seedRailMerchantAccountSecrets.
	return &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull}
}

func storeArmedCharger(dbi *db.DB, msvc *merchants.Service, boot map[string]money.CollectionAdapter, endpoints money.CollectionEndpoints) *money.ScopedCharger {
	ch := money.NewScopedCharger(dbi, boot)
	ch.SetAdapterResolver(&money.MerchantCollectionAdapterBuilder{
		Config:      storeCollectionTestConfig(),
		Rails:       nil, // boot plane empty: credentials exist ONLY in the store
		DB:          dbi,
		MerchantsFn: func() *merchants.Service { return msvc },
		Endpoints:   endpoints,
	})
	return ch
}

func seedArrearsInvoice(t *testing.T, svc *money.MoneyService, ctx context.Context, payer identity.CustomerID, pm uuid.UUID) uuid.UUID {
	t.Helper()
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 50_000))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "store-collection-"+uuid.NewString()[:8], 50_000)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	return inv.ID
}

func TestChargeOutstanding_StoreOnlyStripeCredentials_ChargesThroughStore(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	msvc := merchantsServiceForTest(t, dbi)
	sfx := uuid.NewString()[:8]
	storeKey := "sk_test_store_only_" + sfx
	seedRailMerchantAccountSecrets(t, dbi, msvc, string(models.RailStripe), "acct_store"+sfx, map[string]string{"secret_key": storeKey})

	pm := seedPaymentMethodWithVault(t, pool, ctx, payer, string(models.RailStripe), "pm_store_only_"+sfx)
	seedRailCustomer(t, pool, ctx, payer, string(models.RailStripe), "cus_store_only_"+sfx)
	invID := seedArrearsInvoice(t, svc, ctx, payer, pm)

	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The charge must authenticate with the STORE-resolved key.
		require.Equal(t, "Bearer "+storeKey, r.Header.Get("Authorization"))
		require.NoError(t, r.ParseForm())
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/v1/invoiceitems":
			_, _ = w.Write([]byte(`{"id":"ii_store_only"}`))
		case "/v1/invoices":
			_, _ = w.Write([]byte(`{"id":"in_store_only","status":"draft"}`))
		case "/v1/invoices/in_store_only/finalize":
			_, _ = w.Write([]byte(`{"id":"in_store_only","status":"open","payment_intent":"pi_store_only"}`))
		case "/v1/invoices/in_store_only/pay":
			_, _ = w.Write([]byte(`{"id":"in_store_only","status":"paid","amount_paid":5,"payment_intent":"pi_store_only","charge":"ch_store_only"}`))
		default:
			t.Fatalf("unexpected Stripe path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	// NO boot adapters at all: only store resolution can arm this charge.
	ch := storeArmedCharger(dbi, msvc, nil, money.CollectionEndpoints{StripeBaseURL: server.URL})

	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, calls, 4)

	paid, err := svc.GetInvoiceByID(ctx, payer, invID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(0), paid.AmountDue)
}

func TestChargeOutstanding_StoreOnlyNMICredentials_ChargesThroughStore(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	msvc := merchantsServiceForTest(t, dbi)
	sfx := uuid.NewString()[:8]
	seedRailMerchantAccountSecrets(t, dbi, msvc, string(models.RailNMI), "gw-store-"+sfx, map[string]string{
		"security_key": "store-only-security-key-" + sfx,
	})

	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	invID := seedArrearsInvoice(t, svc, ctx, payer, pm)

	seen := make(chan struct{}, 1)
	// #297: store-armed collections are merchant-initiated stored-credential
	// charges and ride classic Direct Post.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "sale", r.Form.Get("type"))
		// The store-armed client authenticates with the STORE key.
		require.Equal(t, "store-only-security-key-"+sfx, r.Form.Get("security_key"))
		require.Equal(t, "vault_"+pm.String(), r.Form.Get("customer_vault_id"))
		require.Equal(t, "0.05", r.Form.Get("amount"))
		require.Equal(t, "merchant", r.Form.Get("initiated_by"))
		require.Equal(t, "used", r.Form.Get("stored_credential_indicator"))
		seen <- struct{}{}
		_, _ = w.Write([]byte("response=1&responsetext=SUCCESS&authcode=OK&transactionid=txn_store_only_nmi&response_code=100"))
	}))
	t.Cleanup(server.Close)

	ch := storeArmedCharger(dbi, msvc, nil, money.CollectionEndpoints{NMIDirectPostURL: server.URL})

	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("NMI gateway was not called")
	}

	paid, err := svc.GetInvoiceByID(ctx, payer, invID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)

	var railPaymentID string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(rail_payment_id), '')
		FROM openrails.invoice_payments
		WHERE invoice_id = $1 AND status = 'settled'
	`, invID).Scan(&railPaymentID))
	require.Equal(t, "txn_store_only_nmi", railPaymentID)
}

func TestChargeOutstanding_BootPlaneFallback_WhenMerchantDeclaresNoAccount(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	// Real merchants service, but NO rail_merchant_accounts row for the rail:
	// the resolver must decline (ok=false) and the boot adapter must charge.
	msvc := merchantsServiceForTest(t, dbi)

	pm := seedPaymentMethodWithVault(t, pool, ctx, payer, string(models.RailStripe), "pm_boot_fallback")
	seedRailCustomer(t, pool, ctx, payer, string(models.RailStripe), "cus_boot_fallback")
	invID := seedArrearsInvoice(t, svc, ctx, payer, pm)

	boot := &fakeCollectionAdapter{}
	ch := storeArmedCharger(dbi, msvc, map[string]money.CollectionAdapter{
		string(models.RailStripe): boot,
	}, money.CollectionEndpoints{})

	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, boot.charges, 1, "boot-plane adapter must serve merchants with no declared account")

	paid, err := svc.GetInvoiceByID(ctx, payer, invID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
}

func TestChargeOutstanding_DeclaredAccountMissingSecret_FailsClosed(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	msvc := merchantsServiceForTest(t, dbi)
	sfx := uuid.NewString()[:8]
	// Declared account row, NO secrets: never boot fallback, the charge errors.
	seedRailMerchantAccountSecrets(t, dbi, msvc, string(models.RailStripe), "acct_secretless"+sfx, map[string]string{})

	pm := seedPaymentMethodWithVault(t, pool, ctx, payer, string(models.RailStripe), "pm_fail_closed_"+sfx)
	seedRailCustomer(t, pool, ctx, payer, string(models.RailStripe), "cus_fail_closed_"+sfx)
	seedArrearsInvoice(t, svc, ctx, payer, pm)

	boot := &fakeCollectionAdapter{}
	ch := storeArmedCharger(dbi, msvc, map[string]money.CollectionAdapter{
		string(models.RailStripe): boot,
	}, money.CollectionEndpoints{})

	_, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.Error(t, err, "a declared account with a missing secret must fail the charge closed")
	require.Contains(t, err.Error(), "missing")
	require.Empty(t, boot.charges, "fail-closed must never fall back to the boot-plane adapter")
}
