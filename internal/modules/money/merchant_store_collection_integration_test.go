//go:build integration

package money_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
)

// #725: the arrears collection path must arm rail credentials PER MERCHANT
// from the merchant-secrets store at charge time — store wins, boot-plane
// rails only when the merchant declares no account. These tests run the REAL
// ChargeOutstanding path against fake provider HTTP servers with credentials
// that exist ONLY in the store (nothing in the boot plane), plus the boot
// fallback and the fail-closed (declared-but-secretless) contract.

func storeCollectionTestConfig() *config.Config {
	// TestMode=sandbox → provider environment "test", matching
	// merchantsServiceForTest / seedPSPSecrets.
	return &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull, ProviderSandbox: &config.ProviderSandboxConfig{NMIGatewayURL: "http://127.0.0.1:1"}}
}

func storeArmedCharger(dbi *db.DB, msvc *merchants.Service, boot map[string]money.CollectionAdapter, endpoints money.CollectionEndpoints) *money.ScopedCharger {
	ch := money.NewScopedCharger(dbi, boot)
	ch.SetAdapterResolver(&money.MerchantCollectionAdapterBuilder{
		Config:      storeCollectionTestConfig(),
		DB:          dbi,
		MerchantsFn: func() *merchants.Service { return msvc },
		Endpoints:   endpoints,
	})
	return ch
}

func seedArrearsInvoice(t *testing.T, svc *money.MoneyService, ctx context.Context, payer identity.CustomerID, pm uuid.UUID) uuid.UUID {
	t.Helper()
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetInvoiceCollectionPaymentMethod(ctx, payer, money.DefaultCurrency, pm))
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 50_000))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "store-collection-"+uuid.NewString()[:8], 50_000)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	return inv.ID
}

func TestChargeOutstanding_StoreOnlyStripeCredentials_ChargesThroughStore(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	msvc := merchantsServiceForTest(t, dbi)
	sfx := uuid.NewString()[:8]
	storeKey := "sk_test_store_only_" + sfx
	seedPSPSecrets(t, dbi, msvc, string(models.RailStripe), "acct_store"+sfx, map[string]string{"secret_key": storeKey})

	pm := seedPaymentMethodWithRailCustomerRef(t, pool, ctx, payer, string(models.RailStripe), "pm_store_only_"+sfx)
	seedRailCustomer(t, pool, ctx, payer, string(models.RailStripe), "cus_store_only_"+sfx)
	invID := seedArrearsInvoice(t, svc, ctx, payer, pm)

	var calls []string
	var collectionKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The charge must authenticate with the STORE-resolved key.
		require.Equal(t, "Bearer "+storeKey, r.Header.Get("Authorization"))
		require.NoError(t, r.ParseForm())
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/v1/invoiceitems":
			_, _ = w.Write([]byte(`{"id":"ii_store_only"}`))
		case "/v1/invoices":
			collectionKey = r.Form.Get("metadata[openrails_collection_key]")
			_, _ = w.Write([]byte(`{"id":"in_store_only","status":"draft"}`))
		case "/v1/invoices/in_store_only/finalize":
			_, _ = w.Write([]byte(`{"id":"in_store_only","status":"open","payment_intent":"pi_store_only"}`))
		case "/v1/invoices/in_store_only/pay":
			// Stripe echoes the invoice as created: key-stamped, in its currency.
			_, _ = w.Write([]byte(`{"id":"in_store_only","status":"paid","amount_paid":5,"currency":"usd","payment_intent":"pi_store_only","charge":"ch_store_only","metadata":{"openrails_collection_key":"` + collectionKey + `"}}`))
		case "/v1/invoices/in_store_only":
			fmt.Fprintf(w, `{"id":"in_store_only","status":"paid","amount_paid":5,"currency":"usd","customer":"cus_store_only_%s","payment_intent":"pi_store_only","charge":"ch_store_only","metadata":{"openrails_collection_key":"%s"}}`, sfx, collectionKey)
		case "/v1/charges/ch_store_only":
			fmt.Fprintf(w, `{"id":"ch_store_only","payment_intent":"pi_store_only","invoice":"in_store_only","amount_captured":5,"currency":"usd","customer":"cus_store_only_%s","payment_method":"pm_store_only_%s","paid":true,"captured":true,"status":"succeeded"}`, sfx, sfx)
		default:
			t.Fatalf("unexpected Stripe path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	// NO boot adapters at all: only store resolution can arm this charge.
	plane := &money.MerchantCollectionAdapterBuilder{Config: storeCollectionTestConfig(), DB: dbi, MerchantsFn: func() *merchants.Service { return msvc }, Endpoints: money.CollectionEndpoints{StripeBaseURL: server.URL}}
	ch := money.NewScopedCharger(dbi, nil)
	ch.SetAdapterResolver(plane)

	n, err := svc.ChargeOutstanding(ctx, collectionRunner(dbi, ch, plane), 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, []string{"/v1/invoices", "/v1/invoiceitems", "/v1/invoices/in_store_only/finalize", "/v1/invoices/in_store_only/pay", "/v1/invoices/in_store_only", "/v1/charges/ch_store_only"}, calls)

	paid, err := svc.GetInvoiceByID(ctx, payer, invID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(0), paid.AmountDue)
}

func TestChargeOutstanding_StoreOnlyNMICredentials_ChargesThroughStore(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	msvc := merchantsServiceForTest(t, dbi)
	sfx := uuid.NewString()[:8]
	seedPSPSecrets(t, dbi, msvc, string(models.RailNMI), "gw-store-"+sfx, map[string]string{
		"security_key": "store-only-security-key-" + sfx,
	})

	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	invID := seedArrearsInvoice(t, svc, ctx, payer, pm)

	seen := make(chan struct{}, 1)
	var order string
	// #297: store-armed collections are merchant-initiated stored-credential
	// charges and ride classic Direct Post.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/payments/") {
			require.Equal(t, "store-only-security-key-"+sfx, r.Header.Get("Authorization"))
			fmt.Fprintf(w, `{"id":"txn_store_only_nmi","response":"1","currency":"USD","customer_vault_id":"vault_%s","actions":[{"type":"sale","success":true,"amount":"0.05"}]}`, pm)
			return
		}
		require.NoError(t, r.ParseForm())
		// The store-armed client authenticates with the STORE key.
		require.Equal(t, "store-only-security-key-"+sfx, r.Form.Get("security_key"))
		if r.Form.Get("type") != "sale" {
			require.Equal(t, order, r.Form.Get("order_id"))
			fmt.Fprintf(w, `<nm_response><transaction><transaction_id>txn_store_only_nmi</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, order)
			return
		}
		require.Equal(t, "sale", r.Form.Get("type"))
		order = r.Form.Get("orderid")
		require.Equal(t, "vault_"+pm.String(), r.Form.Get("customer_vault_id"))
		require.Equal(t, "0.05", r.Form.Get("amount"))
		require.Equal(t, "merchant", r.Form.Get("initiated_by"))
		require.Equal(t, "used", r.Form.Get("stored_credential_indicator"))
		require.Equal(t, "txn_unscheduled_initial_"+pm.String(), r.Form.Get("initial_transaction_id"))
		seen <- struct{}{}
		_, _ = w.Write([]byte("response=1&responsetext=SUCCESS&authcode=OK&transactionid=txn_store_only_nmi&response_code=100"))
	}))
	t.Cleanup(server.Close)

	plane := &money.MerchantCollectionAdapterBuilder{Config: storeCollectionTestConfig(), DB: dbi, MerchantsFn: func() *merchants.Service { return msvc }, Endpoints: money.CollectionEndpoints{NMIDirectPostURL: server.URL, NMIQueryURL: server.URL, NMIV5BaseURL: server.URL}}
	ch := money.NewScopedCharger(dbi, nil)
	ch.SetAdapterResolver(plane)

	n, err := svc.ChargeOutstanding(ctx, collectionRunner(dbi, ch, plane), 0)
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
		FROM billing.invoice_payments
		WHERE invoice_id = $1 AND status = 'settled'
	`, invID).Scan(&railPaymentID))
	require.Equal(t, "txn_store_only_nmi", railPaymentID)
}

// or#893 deleted TestChargeOutstanding_BootPlaneFallback_WhenMerchantDeclaresNoAccount.
// It asserted that a merchant declaring no account still charges, through the
// boot-config adapter. That is a fail-OPEN credential path, and psp_id being
// required makes its premise unreachable: an instrument names the PSP that
// vaulted it, and mode-1 boot arms real psps rows from the manifest, so
// "declares no account" cannot coexist with a chargeable instrument. The
// refusal is now the behaviour, covered by the fail-closed test below.

func TestChargeOutstanding_DeclaredAccountMissingSecret_FailsClosed(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	msvc := merchantsServiceForTest(t, dbi)
	sfx := uuid.NewString()[:8]
	// Declared account row, NO secrets: never boot fallback, the charge errors.
	seedPSPSecrets(t, dbi, msvc, string(models.RailStripe), "acct_secretless"+sfx, map[string]string{})

	pm := seedPaymentMethodWithRailCustomerRef(t, pool, ctx, payer, string(models.RailStripe), "pm_fail_closed_"+sfx)
	seedRailCustomer(t, pool, ctx, payer, string(models.RailStripe), "cus_fail_closed_"+sfx)
	invID := seedArrearsInvoice(t, svc, ctx, payer, pm)

	boot := &fakeCollectionAdapter{}
	ch := storeArmedCharger(dbi, msvc, map[string]money.CollectionAdapter{
		string(models.RailStripe): boot,
	}, money.CollectionEndpoints{})

	// A declared account with a missing secret fails closed BEFORE any
	// submission: the operation parks with the reason, nothing is charged, and
	// no boot-plane adapter is consulted.
	n, err := svc.ChargeOutstanding(ctx, collectionRunner(dbi, ch, nil), 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Empty(t, boot.charges, "fail-closed must never fall back to the boot-plane adapter")
	op := latestCollectionIntent(t, pool, ctx, invID)
	require.Equal(t, intents.StatusPending, op.Status)
	require.Contains(t, *op.LastFailureReason, "missing")
}

// TestInvoiceCollection_DelayedNMIReceiptNeverResubmits: the store-armed
// plane through the real builder. An uncertain sale answer and an empty order
// search keep the operation unknown across restart with no second send; once
// the sale becomes visible AND reads back exactly, the verifier settles once.
func TestInvoiceCollection_DelayedNMIReceiptNeverResubmits(t *testing.T) {
	e := nmiReceiptScenario(t)
	transaction := "invoice-delayed-" + uuid.NewString()
	require.Equal(t, []string{e.op.String()}, e.gateway.sentOrderIDs(), "the wire order id is the operation id")

	// Restart: an empty search keeps the operation unknown; the sweep skips it.
	restarted := money.NewMoneyService(e.db)
	require.Equal(t, intents.StatusUnknownNeedsVerify, e.verify(t))
	_, err := restarted.ChargeOutstanding(e.ctx, e.runner, 0)
	require.NoError(t, err)
	e.requireStillUnknown(t, "empty search")

	e.gateway.orderSale(e.op.String(), transaction)
	e.gateway.payment(transaction, e.vault, "0.05", "USD")
	require.Equal(t, intents.StatusSucceeded, e.verify(t))
	invoice, err := restarted.GetInvoiceByID(e.ctx, e.payer, e.invoice)
	require.NoError(t, err)
	require.Equal(t, "paid", invoice.Status)
	require.Nil(t, invoice.CollectionIntentID)
	_, err = restarted.ChargeOutstanding(e.ctx, e.runner, 0)
	require.NoError(t, err)
	require.Equal(t, 1, e.gateway.sends)
	e.requireSettledOnce(t)
	var railPaymentID string
	require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT rail_payment_id FROM billing.invoice_payments WHERE invoice_id = $1 AND status = 'settled'`, e.invoice).Scan(&railPaymentID))
	require.Equal(t, transaction, railPaymentID)
}
