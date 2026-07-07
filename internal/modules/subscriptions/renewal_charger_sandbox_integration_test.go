//go:build integration

package subscriptions

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
)

// TestRenewalCharger_NMISandbox_SameAnchorAcrossReprice is the opt-in proof
// for #773 + #297 Phase A: it drives the REAL RenewalCharger against the LIVE
// NMI sandbox gateway — a genuine renewal charge, then a #773-scheduled
// reprice, then a SECOND genuine renewal charge — and asserts the SAME
// stored-credential recurring anchor is replayed while only the amount
// changes. Mirrors internal/modules/money's
// TestChargeOutstanding_NMISandbox_CollectsRealCharge opt-in contract:
//   - SKIP cleanly when NMI_SANDBOX_SECURITY_KEY is unset.
//   - FAIL LOUD on a real provider error when it IS set but the path is broken.
func TestRenewalCharger_NMISandbox_SameAnchorAcrossReprice(t *testing.T) {
	securityKey := strings.TrimSpace(os.Getenv("NMI_SANDBOX_SECURITY_KEY"))
	if securityKey == "" {
		t.Skip("NMI_SANDBOX_SECURITY_KEY not set; skipping opt-in NMI sandbox reprice-renewal proof (#773/#297)")
	}

	ctx := dbtest.WithTestMerchant(context.Background())
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	merchantID := dbtest.TestMerchantID.UUID()

	client, err := nmi.NewClient(string(models.RailNMI), &config.NMIProviderSettings{SecurityKey: securityKey}, true)
	require.NoError(t, err)
	require.Equal(t, nmi.DefaultDirectPostURL, client.DirectPostURL, "must hit the real NMI gateway, not a stub")

	vaultID := createRenewalSandboxVault(t, client)
	t.Cleanup(func() {
		if derr := client.DeleteCustomerVault(nmi.DeleteCustomerVaultData{CustomerVaultID: vaultID}); derr != nil {
			t.Logf("sandbox vault cleanup (non-fatal): %v", derr)
		}
	})

	suffix := uuid.NewString()[:8]
	productID := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO openrails.products (id, merchant_id, key, display_name) VALUES ($1,$2,$3,$3)`,
		productID, merchantID, "reprice-sandbox-product-"+suffix)
	require.NoError(t, err)

	lowPriceID, highPriceID := uuid.New(), uuid.New()
	insertPrice := func(id uuid.UUID, amountMicros int64, key string) {
		_, e := pool.Exec(ctx, `
			INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency, access_duration_hours, auto_renew, archived, key)
			VALUES ($1,$2,$3,$4,'usd',720,true,false,$5)`,
			id, productID, merchantID, amountMicros, key)
		require.NoError(t, e)
	}
	// $1.10-$1.89 band (matches the sandbox simulator's approving amounts —
	// same band the #619 collection sandbox proof uses).
	base := (int64(110) + time.Now().UnixNano()%80) * 10_000 // internal micros
	insertPrice(lowPriceID, base, "reprice-sandbox-low-"+suffix)
	insertPrice(highPriceID, base+50_0000, "reprice-sandbox-high-"+suffix)

	subject := "reprice-sandbox-customer-" + suffix
	var customerID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO openrails.customers (merchant_id, subject) VALUES ($1,$2) RETURNING id`,
		merchantID, subject).Scan(&customerID))

	pmID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.payment_methods (id, merchant_id, customer_id, rail, rail_customer_ref, initial_transaction_id)
		VALUES ($1,$2,$3,'nmi',$4,$5)`,
		pmID, merchantID, customerID, vaultID, "init-"+pmID.String())
	require.NoError(t, err)

	railSubID := "reprice-sandbox-rail-sub-" + suffix
	now := time.Now().UTC()
	var subID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO openrails.subscriptions (merchant_id, customer_id, product_id, price_id, status, rail, rail_subscription_id, payment_method_id, current_period_starts_at, current_period_ends_at)
		VALUES ($1,$2,$3,$4,'active','nmi',$5,$6,$7,$8) RETURNING id`,
		merchantID, customerID, productID, lowPriceID, railSubID, pmID, now, now.Add(30*24*time.Hour)).Scan(&subID))

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM openrails.subscription_reprices WHERE merchant_id = $1", merchantID)
		_, _ = pool.Exec(bg, "DELETE FROM openrails.payments WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(bg, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(bg, "DELETE FROM openrails.payment_methods WHERE id = $1", pmID)
		_, _ = pool.Exec(bg, "DELETE FROM openrails.prices WHERE product_id = $1", productID)
		_, _ = pool.Exec(bg, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi)
	subSvc := NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil)
	lifecycle := NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc)
	repriceSvc := NewRepriceService(dbi, NewRepriceRepo(dbi), priceSvc, subSvc, notifSvc, nil)
	rc := &RenewalCharger{Charger: nmidirect.New(client), Reprice: repriceSvc, Lifecycle: lifecycle, DB: dbi}

	loadPM := func() *models.PaymentMethod {
		var railCustomerRef, recurringRef string
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT rail_customer_ref, stored_credential_recurring_ref FROM openrails.payment_methods WHERE id = $1`, pmID).
			Scan(&railCustomerRef, &recurringRef))
		return &models.PaymentMethod{ID: pmID, Rail: models.RailNMI, RailCustomerRef: railCustomerRef, StoredCredentialRecurringRef: recurringRef}
	}
	loadSub := func() *models.Subscription {
		sub, err := subSvc.GetByID(ctx, subID)
		require.NoError(t, err)
		return sub
	}

	// ACT 1: first real renewal charge — a legacy/never-anchored instrument
	// charges reference-less and the success back-fills the anchor.
	res1, err := rc.ChargeRenewal(ctx, loadSub(), loadPM())
	require.NoError(t, err, "real NMI sandbox renewal charge failed (configured but broken: fail loud, do not skip)")
	require.NotEmpty(t, res1.TransactionID)

	pmAfterFirst := loadPM()
	require.NotEmpty(t, pmAfterFirst.StoredCredentialRecurringRef, "first successful renewal must anchor the recurring sequence (#297 write-once backfill)")
	anchor := pmAfterFirst.StoredCredentialRecurringRef

	// Schedule a #773 reprice effective now.
	_, err = repriceSvc.Reprice(ctx, RepriceRequest{SubscriptionID: subID, ToPriceID: highPriceID, EffectiveAt: time.Now().UTC()})
	require.NoError(t, err)

	// ACT 2: second real renewal charge — the due reprice flips the CHARGED
	// amount; the SAME anchor is replayed (write-once, unchanged).
	res2, err := rc.ChargeRenewal(ctx, loadSub(), loadPM())
	require.NoError(t, err, "real NMI sandbox reprice-driven renewal charge failed")
	require.NotEmpty(t, res2.TransactionID)
	require.NotEqual(t, res1.TransactionID, res2.TransactionID)

	pmAfterSecond := loadPM()
	require.Equal(t, anchor, pmAfterSecond.StoredCredentialRecurringRef, "SAME stored-credential anchor across the reprice")

	finalSub := loadSub()
	require.Equal(t, highPriceID, finalSub.PriceID, "subscription re-pinned to the repriced price")
}

func createRenewalSandboxVault(t *testing.T, client *nmi.NMIClient) string {
	t.Helper()
	values := url.Values{
		"customer_vault": {"add_customer"},
		"security_key":   {client.SecurityKey},
		"ccnumber":       {"4111111111111111"},
		"ccexp":          {"1028"},
		"cvv":            {"999"},
		"first_name":     {"OpenRails"},
		"last_name":      {"RepriceTest"},
		"address1":       {"888"},
		"zip":            {"77777"},
		"orderid":        {"openrails-reprice-vault-" + time.Now().UTC().Format("20060102150405.000000000")},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, client.DirectPostURL, strings.NewReader(values.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	output, err := url.ParseQuery(string(body))
	require.NoError(t, err)
	if output.Get("response") != "1" {
		t.Fatalf("NMI vault create failed: response=%s text=%s code=%s", output.Get("response"), output.Get("responsetext"), output.Get("response_code"))
	}
	vaultID := strings.TrimSpace(output.Get("customer_vault_id"))
	require.NotEmpty(t, vaultID)
	return vaultID
}
