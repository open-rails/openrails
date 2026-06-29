//go:build integration

package money_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// moneyInEnv provisions a fresh payer with NO initial deposit (balance 0).
func moneyInEnv(t *testing.T) (*money.MoneyService, *pgxpool.Pool, identity.CustomerID, string, context.Context) {
	svc, _, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	return svc, pool, payer, currency, ctx
}

func moneyInEnvWithDB(t *testing.T) (*money.MoneyService, *db.DB, *pgxpool.Pool, identity.CustomerID, string, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()

	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		// money_spend_limits (#513 B1), money_blocks + money_transactions (#512)
		// were dropped; only money_settings remains to reset.
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
	})
	return money.NewMoneyService(dbi), dbi, pool, payer, money.DefaultCurrency, ctx
}

// --- fakes ---

type fakeCharger struct {
	charges    []money.ChargeRequest
	declineAll bool
}

func (f *fakeCharger) ChargeSavedMethod(_ context.Context, req money.ChargeRequest) (money.ChargeResult, error) {
	f.charges = append(f.charges, req)
	if f.declineAll {
		return money.ChargeResult{Declined: true}, nil
	}
	return money.ChargeResult{TransactionID: "tx_" + req.IdempotencyKey}, nil
}

type fakeCollectionAdapter struct {
	charges           []money.ChargeRequest
	methods           []gen.OpenrailsPaymentMethod
	decline           bool
	transientFailures int
}

func (f *fakeCollectionAdapter) ChargeSavedMethod(_ context.Context, method gen.OpenrailsPaymentMethod, req money.ChargeRequest) (money.ChargeResult, error) {
	f.methods = append(f.methods, method)
	f.charges = append(f.charges, req)
	if f.transientFailures > 0 {
		f.transientFailures--
		return money.ChargeResult{}, errors.New("gateway timeout")
	}
	if f.decline {
		code := "card_declined"
		message := "card declined"
		return money.ChargeResult{Rail: method.Rail, TransactionID: "declined_" + req.IdempotencyKey, Declined: true, FailureCode: &code, FailureMessage: &message}, nil
	}
	return money.ChargeResult{Rail: method.Rail, TransactionID: "tx_" + req.IdempotencyKey}, nil
}

type fakeAlerter struct{ calls int }

func (f *fakeAlerter) LowBalanceAlert(_ context.Context, _ identity.CustomerID, _, _ int64) error {
	f.calls++
	return nil
}

// latestBlockExpiry returns the expiry of the most recent credit lot (a #514
// credit grant) for the payer — the money_blocks table is gone (#512 hard cut),
// the credit grant carries the lot's amount + expiry.
func latestBlockExpiry(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payerID uuid.UUID) *time.Time {
	t.Helper()
	var exp *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT ends_at FROM openrails.grants WHERE customer_id = $1 AND kind = 'credit' AND event = 'grant' ORDER BY created_at DESC LIMIT 1",
		payerID).Scan(&exp))
	return exp
}

func seedPaymentMethod(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID, rail string) uuid.UUID {
	t.Helper()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, payer.UUID().String())
	pm := uuid.New()
	return seedPaymentMethodRow(t, pool, ctx, payer, rail, pm, "vault_"+pm.String())
}

func seedPaymentMethodWithVault(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID, rail, vaultID string) uuid.UUID {
	t.Helper()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, payer.UUID().String())
	// One row with the requested rail + vault. (Was double-inserting the
	// same id via seedPaymentMethod first → duplicate payment_methods_pkey.)
	return seedPaymentMethodRow(t, pool, ctx, payer, rail, uuid.New(), vaultID)
}

func seedPaymentMethodRow(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID, rail string, pm uuid.UUID, vaultID string) uuid.UUID {
	t.Helper()
	params := gen.CreatePaymentMethodParams{
		ID:                   pm,
		MerchantID:           dbtest.TestMerchantID.UUID(),
		CustomerID:           payer.UUID(),
		Rail:                 rail,
		InitialTransactionID: "init_" + pm.String(),
	}
	// Mirror the migration's per-rail handle placement: NMI keeps the customer
	// vault in rail_customer_ref; other rails put the instrument in rail_method_ref.
	if rails.IsNMIBacked(rail) {
		params.RailCustomerRef = vaultID
	} else {
		params.RailMethodRef = vaultID
	}
	_, err := gen.New(pool).CreatePaymentMethod(ctx, params)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", pm)
	})
	return pm
}

func seedRailCustomer(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID, rail, railCustomerID string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, gen.New(pool).UpsertRailCustomer(ctx, gen.UpsertRailCustomerParams{
		ID:             uuidutil.NewV7(),
		MerchantID:     dbtest.TestMerchantID.UUID(),
		CustomerID:     payer.UUID(),
		Rail:           rail,
		RailCustomerID: railCustomerID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}))
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_customers WHERE merchant_id = $1 AND customer_id = $2 AND rail = $3", dbtest.TestMerchantID.UUID(), payer.UUID(), rail)
	})
}

// --- #240 expiry default ---

func TestDeposit_DefaultExpiry_NoSettingsRow(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1000,
		Source: "purchase", ApplyAccountExpiryDefault: true,
	})
	require.NoError(t, err)
	exp := latestBlockExpiry(t, pool, ctx, payer.UUID())
	require.NotNil(t, exp, "default 365d expiry should be applied")
	days := exp.Sub(time.Now().UTC()).Hours() / 24
	require.InDelta(t, 365, days, 1.5)
}

func TestDeposit_NoFlag_Permanent(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1000, Source: "grant",
	})
	require.NoError(t, err)
	exp := latestBlockExpiry(t, pool, ctx, payer.UUID())
	require.Nil(t, exp, "no flag, no explicit expiry -> permanent")
}

func TestDeposit_ConfiguredExpiryHours(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	hours := 30 * 24
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{DefaultCreditExpiryHours: &hours})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1000,
		Source: "purchase", ApplyAccountExpiryDefault: true,
	})
	require.NoError(t, err)
	exp := latestBlockExpiry(t, pool, ctx, payer.UUID())
	require.NotNil(t, exp)
	require.InDelta(t, 30, exp.Sub(time.Now().UTC()).Hours()/24, 1.5)
}

// --- #240 low-balance alerts ---

func TestRunLowBalanceAlerts(t *testing.T) {
	svc, _, payer, _, ctx := moneyInEnv(t)
	thr := int64(1000)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{LowBalanceThreshold: &thr})
	require.NoError(t, err)
	// available 500 < 1000
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	al := &fakeAlerter{}
	n, err := svc.RunLowBalanceAlerts(ctx, al, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, al.calls)

	// Re-run within cooldown -> deduped.
	n2, err := svc.RunLowBalanceAlerts(ctx, al, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 0, n2)
	require.Equal(t, 1, al.calls)
}

// --- #239 auto-top-up ---

func TestRunAutoTopups_ChargesAndDeposits(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	thr, amt := int64(1000), int64(50_000_000)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailMobius))
	enabled := true
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		LowBalanceThreshold: &thr, AutoTopupEnabled: &enabled, AutoTopupAmountCents: &amt, AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	ch := &fakeCharger{}
	n, err := svc.RunAutoTopups(ctx, ch, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ch.charges, 1)
	// auto_topup_amount is stored in native units; the rail receives cents.
	require.Equal(t, int64(5000), ch.charges[0].AmountCents)
	require.Equal(t, pm, ch.charges[0].PaymentMethodID)

	bal, err := svc.GetBalanceForCustomer(ctx, payer, currency)
	require.NoError(t, err)
	// Ledger uses internal units: 500 seed + configured native-unit top-up deposited.
	require.Equal(t, int64(50_000_500), bal.Balance)

	// Re-run within cooldown: no second charge.
	n2, err := svc.RunAutoTopups(ctx, ch, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 0, n2)
	require.Len(t, ch.charges, 1)
}

func TestRunAutoTopups_Declined(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	thr, amt := int64(1000), int64(50_000_000)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailMobius))
	enabled := true
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		LowBalanceThreshold: &thr, AutoTopupEnabled: &enabled, AutoTopupAmountCents: &amt, AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	ch := &fakeCharger{declineAll: true}
	n, err := svc.RunAutoTopups(ctx, ch, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, ch.charges, 1, "charge attempted")
	bal, err := svc.GetBalanceForCustomer(ctx, payer, currency)
	require.NoError(t, err)
	require.Equal(t, int64(500), bal.Balance, "declined -> no deposit")
}

func TestScopedCharger_ValidatesPaymentMethodScopeAndDispatches(t *testing.T) {
	_, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailMobius))
	adapter := &fakeCollectionAdapter{}
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailMobius): adapter,
	})

	res, err := ch.ChargeSavedMethod(ctx, money.ChargeRequest{
		MerchantID:      dbtest.TestMerchantID.UUID(),
		Payer:           payer,
		Invoker:         payer.UUID().String(),
		PaymentMethodID: pm,
		AmountCents:     123,
		IdempotencyKey:  "scope-ok",
		Description:     "invoice",
	})
	require.NoError(t, err)
	require.Equal(t, string(models.RailMobius), res.Rail)
	require.Equal(t, "tx_scope-ok", res.TransactionID)
	require.Len(t, adapter.charges, 1)
	require.Equal(t, pm, adapter.charges[0].PaymentMethodID)

	otherPayer := identity.CustomerIDFromString(uuid.NewString())
	_, err = ch.ChargeSavedMethod(ctx, money.ChargeRequest{
		MerchantID:      dbtest.TestMerchantID.UUID(),
		Payer:           otherPayer,
		PaymentMethodID: pm,
		AmountCents:     123,
		IdempotencyKey:  "wrong-customer",
	})
	require.ErrorContains(t, err, "another customer")
	require.Len(t, adapter.charges, 1, "scope failure must not dispatch")

	_, err = ch.ChargeSavedMethod(ctx, money.ChargeRequest{
		MerchantID:      uuid.New(),
		Payer:           payer,
		PaymentMethodID: pm,
		AmountCents:     123,
		IdempotencyKey:  "wrong-merchant",
	})
	require.ErrorContains(t, err, "another merchant")
	require.Len(t, adapter.charges, 1, "merchant scope failure must not dispatch")
}

func TestScopedCharger_RejectsUnsupportedRail(t *testing.T) {
	_, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailCCBill))
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailCCBill): &fakeCollectionAdapter{},
	})

	_, err := ch.ChargeSavedMethod(ctx, money.ChargeRequest{
		MerchantID:      dbtest.TestMerchantID.UUID(),
		Payer:           payer,
		PaymentMethodID: pm,
		AmountCents:     123,
		IdempotencyKey:  "ccbill",
	})
	require.ErrorContains(t, err, "does not support invoice collection")
}

func TestScopedCharger_NMIAdapterCollectsThroughGateway(t *testing.T) {
	_, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailMobius))
	seen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "sale", r.Form.Get("type"))
		require.Equal(t, "test-security-key", r.Form.Get("security_key"))
		require.Equal(t, "vault_"+pm.String(), r.Form.Get("customer_vault_id"))
		require.Equal(t, "1.23", r.Form.Get("amount"))
		require.Equal(t, money.DefaultCurrency, r.Form.Get("currency"))
		require.Equal(t, "nmi-scope-ok", r.Form.Get("orderid"))
		require.Equal(t, "invoice", r.Form.Get("order_description"))
		seen <- struct{}{}
		_, _ = w.Write([]byte("response=1&responsetext=SUCCESS&transactionid=txn_nmi_invoice_123&response_code=100"))
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewClient(string(models.RailMobius), &config.NMIProviderSettings{SecurityKey: "test-security-key"}, false)
	require.NoError(t, err)
	client.DirectPostURL = server.URL
	ch := money.NewScopedCharger(dbi, money.NewNMICollectionAdapters(map[string]*nmi.NMIClient{
		string(models.RailMobius): client,
	}))

	res, err := ch.ChargeSavedMethod(ctx, money.ChargeRequest{
		MerchantID:      dbtest.TestMerchantID.UUID(),
		Payer:           payer,
		Invoker:         payer.UUID().String(),
		PaymentMethodID: pm,
		AmountCents:     123,
		Currency:        money.DefaultCurrency,
		IdempotencyKey:  "nmi-scope-ok",
		Description:     "invoice",
	})
	require.NoError(t, err)
	require.Equal(t, string(models.RailMobius), res.Rail)
	require.Equal(t, "txn_nmi_invoice_123", res.TransactionID)
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("NMI gateway was not called")
	}
}

func TestScopedCharger_NMIAdapterDeclineReturnsStructuredFailure(t *testing.T) {
	_, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailMobius))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		_, _ = w.Write([]byte("response=2&responsetext=Do not honor&transactionid=txn_declined&response_code=201"))
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewClient(string(models.RailMobius), &config.NMIProviderSettings{SecurityKey: "test-security-key"}, false)
	require.NoError(t, err)
	client.DirectPostURL = server.URL
	ch := money.NewScopedCharger(dbi, money.NewNMICollectionAdapters(map[string]*nmi.NMIClient{
		string(models.RailMobius): client,
	}))

	res, err := ch.ChargeSavedMethod(ctx, money.ChargeRequest{
		MerchantID:      dbtest.TestMerchantID.UUID(),
		Payer:           payer,
		PaymentMethodID: pm,
		AmountCents:     123,
		Currency:        money.DefaultCurrency,
		IdempotencyKey:  "nmi-decline",
	})
	require.NoError(t, err)
	require.True(t, res.Declined)
	require.Equal(t, string(models.RailMobius), res.Rail)
	require.NotNil(t, res.FailureCode)
	require.Equal(t, "do_not_honor", *res.FailureCode)
	require.NotNil(t, res.FailureMessage)
	require.Contains(t, *res.FailureMessage, "Do not honor")
}

func TestChargeOutstanding_WithNMIAdapter_SettlesInvoiceThroughGateway(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailMobius))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 50_000))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "nmi-invoice-collection", 50_000)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	seen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "sale", r.Form.Get("type"))
		require.Equal(t, "vault_"+pm.String(), r.Form.Get("customer_vault_id"))
		require.Equal(t, "0.05", r.Form.Get("amount"))
		require.Equal(t, "invoice:"+inv.ID.String()+":50000", r.Form.Get("orderid"))
		seen <- struct{}{}
		_, _ = w.Write([]byte("response=1&responsetext=SUCCESS&transactionid=txn_nmi_invoice_settled&response_code=100"))
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewClient(string(models.RailMobius), &config.NMIProviderSettings{SecurityKey: "test-security-key"}, false)
	require.NoError(t, err)
	client.DirectPostURL = server.URL
	ch := money.NewScopedCharger(dbi, money.NewNMICollectionAdapters(map[string]*nmi.NMIClient{
		string(models.RailMobius): client,
	}))

	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("NMI gateway was not called")
	}

	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(50_000), paid.AmountPaid)
	require.Equal(t, int64(0), paid.AmountDue)

	var railPaymentID string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(rail_payment_id), '')
		FROM openrails.invoice_payments
		WHERE invoice_id = $1 AND status = 'settled'
	`, inv.ID).Scan(&railPaymentID))
	require.Equal(t, "txn_nmi_invoice_settled", railPaymentID)
}

func TestChargeOutstanding_WithStripeAdapter_SettlesInvoiceThroughStripeServer(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := seedPaymentMethodWithVault(t, pool, ctx, payer, string(models.RailStripe), "pm_openrails_invoice")
	seedRailCustomer(t, pool, ctx, payer, string(models.RailStripe), "cus_openrails_invoice")
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 50_000))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "stripe-invoice-collection", 50_000)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer sk_test_invoice", r.Header.Get("Authorization"))
		require.NoError(t, r.ParseForm())
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/v1/invoiceitems":
			require.Equal(t, "cus_openrails_invoice", r.Form.Get("customer"))
			require.Equal(t, "5", r.Form.Get("amount"))
			require.Equal(t, "usd", r.Form.Get("currency"))
			require.Equal(t, inv.ID.String(), r.Form.Get("metadata[openrails_invoice_id]"))
			require.Equal(t, "invoice:"+inv.ID.String()+":50000:invoice_item", r.Header.Get("Idempotency-Key"))
			_, _ = w.Write([]byte(`{"id":"ii_openrails_invoice"}`))
		case "/v1/invoices":
			require.Equal(t, "cus_openrails_invoice", r.Form.Get("customer"))
			require.Equal(t, "charge_automatically", r.Form.Get("collection_method"))
			require.Equal(t, "pm_openrails_invoice", r.Form.Get("default_payment_method"))
			require.Equal(t, "include", r.Form.Get("pending_invoice_items_behavior"))
			require.Equal(t, "invoice:"+inv.ID.String()+":50000:invoice", r.Header.Get("Idempotency-Key"))
			_, _ = w.Write([]byte(`{"id":"in_openrails_invoice","status":"draft"}`))
		case "/v1/invoices/in_openrails_invoice/finalize":
			require.Equal(t, "invoice:"+inv.ID.String()+":50000:finalize", r.Header.Get("Idempotency-Key"))
			_, _ = w.Write([]byte(`{"id":"in_openrails_invoice","status":"open","payment_intent":"pi_openrails_invoice"}`))
		case "/v1/invoices/in_openrails_invoice/pay":
			require.Equal(t, "pm_openrails_invoice", r.Form.Get("payment_method"))
			require.Equal(t, "invoice:"+inv.ID.String()+":50000:pay", r.Header.Get("Idempotency-Key"))
			_, _ = w.Write([]byte(`{"id":"in_openrails_invoice","status":"paid","amount_paid":5,"payment_intent":"pi_openrails_invoice","charge":"ch_openrails_invoice"}`))
		default:
			t.Fatalf("unexpected Stripe path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	stripeSvc := &subscriptions.StripeService{
		Config: &config.Config{},
		Rails: config.RailSet{
			"stripe": {Type: config.RailTypeStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_invoice"}},
		},
	}
	stripeSvc.SetBaseURLForTest(server.URL)
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailStripe): money.NewStripeCollectionAdapter(dbi, stripeSvc),
	})

	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, []string{
		"/v1/invoiceitems",
		"/v1/invoices",
		"/v1/invoices/in_openrails_invoice/finalize",
		"/v1/invoices/in_openrails_invoice/pay",
	}, calls)

	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(50_000), paid.AmountPaid)
	require.Equal(t, int64(0), paid.AmountDue)
	require.NotNil(t, paid.ExternalInvoiceID)
	require.Equal(t, "in_openrails_invoice", *paid.ExternalInvoiceID)

	var rail, railPaymentID string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(rail), ''), COALESCE(MAX(rail_payment_id), '')
		FROM openrails.invoice_payments
		WHERE invoice_id = $1 AND status = 'settled'
	`, inv.ID).Scan(&rail, &railPaymentID))
	require.Equal(t, string(models.RailStripe), rail)
	require.Equal(t, "ch_openrails_invoice", railPaymentID)

	n, err = svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, []string{
		"/v1/invoiceitems",
		"/v1/invoices",
		"/v1/invoices/in_openrails_invoice/finalize",
		"/v1/invoices/in_openrails_invoice/pay",
	}, calls)
	var settledPayments int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		FROM openrails.invoice_payments
		WHERE invoice_id = $1 AND status = 'settled'
	`, inv.ID).Scan(&settledPayments))
	require.Equal(t, 1, settledPayments)
}

func TestChargeOutstanding_WithStripeAdapter_DeclineRecordsFailure(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := seedPaymentMethodWithVault(t, pool, ctx, payer, string(models.RailStripe), "pm_openrails_decline")
	seedRailCustomer(t, pool, ctx, payer, string(models.RailStripe), "cus_openrails_decline")
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 50_000))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "stripe-invoice-decline", 50_000)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		switch r.URL.Path {
		case "/v1/invoiceitems":
			_, _ = w.Write([]byte(`{"id":"ii_openrails_decline"}`))
		case "/v1/invoices":
			_, _ = w.Write([]byte(`{"id":"in_openrails_decline","status":"draft"}`))
		case "/v1/invoices/in_openrails_decline/finalize":
			_, _ = w.Write([]byte(`{"id":"in_openrails_decline","status":"open","payment_intent":"pi_openrails_decline"}`))
		case "/v1/invoices/in_openrails_decline/pay":
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"Your card was declined.","code":"card_declined","decline_code":"do_not_honor"}}`))
		default:
			t.Fatalf("unexpected Stripe path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	stripeSvc := &subscriptions.StripeService{
		Config: &config.Config{},
		Rails: config.RailSet{
			"stripe": {Type: config.RailTypeStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_invoice"}},
		},
	}
	stripeSvc.SetBaseURLForTest(server.URL)
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailStripe): money.NewStripeCollectionAdapter(dbi, stripeSvc),
	})

	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)

	stillOpen, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "open", stillOpen.Status)
	require.Equal(t, int64(50_000), stillOpen.AmountDue)

	var rail, failureCode, failureMessage string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(rail, ''), COALESCE(failure_code, ''), COALESCE(failure_message, '')
		FROM openrails.invoice_payments
		WHERE invoice_id = $1 AND status = 'failed'
		ORDER BY created_at DESC
		LIMIT 1
	`, inv.ID).Scan(&rail, &failureCode, &failureMessage))
	require.Equal(t, string(models.RailStripe), rail)
	require.Equal(t, "do_not_honor", failureCode)
	require.Contains(t, failureMessage, "Your card was declined.")
}

func TestChargeOutstanding_WithScopedCharger_SettlesInvoiceAndRecordsRail(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailMobius))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 1_000))
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1_000, Source: "seed"})
	require.NoError(t, err)
	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{Payer: &payer, Invoker: "u", Currency: money.DefaultCurrency, EventType: "gpt-4o", Amount: 1_500, Source: "req", SourceID: "scoped-invoice-charge"})
	require.NoError(t, err)

	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, int64(500), inv.AmountDue)

	adapter := &fakeCollectionAdapter{}
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailMobius): adapter,
	})
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, adapter.charges, 1)
	require.Equal(t, dbtest.TestMerchantID.UUID(), adapter.charges[0].MerchantID)
	require.NotNil(t, adapter.charges[0].InvoiceID)
	require.Equal(t, inv.ID, *adapter.charges[0].InvoiceID)
	require.Equal(t, pm, adapter.charges[0].PaymentMethodID)
	require.Equal(t, int64(1), adapter.charges[0].AmountCents)

	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(500), paid.AmountPaid)
	require.Equal(t, int64(0), paid.AmountDue)

	var rail, railPaymentID string
	var paymentCount int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(MAX(rail), ''), COALESCE(MAX(rail_payment_id), '')
		FROM openrails.invoice_payments
		WHERE invoice_id = $1 AND status = 'settled'
	`, inv.ID).Scan(&paymentCount, &rail, &railPaymentID))
	require.Equal(t, 1, paymentCount)
	require.Equal(t, string(models.RailMobius), rail)
	require.Equal(t, "tx_invoice:"+inv.ID.String()+":500", railPaymentID)

	n, err = svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, adapter.charges, 1, "paid invoice must not be recharged")
}

func TestChargeOutstanding_WithScopedCharger_DeclineRecordsFailureMetadata(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailMobius))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 500))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "scoped-decline", 500)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	adapter := &fakeCollectionAdapter{decline: true}
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailMobius): adapter,
	})
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, adapter.charges, 1)

	stillOpen, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "open", stillOpen.Status)
	require.Equal(t, int64(500), stillOpen.AmountDue)

	var rail, railPaymentID, failureCode, failureMessage string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(rail, ''), COALESCE(rail_payment_id, ''), COALESCE(failure_code, ''), COALESCE(failure_message, '')
		FROM openrails.invoice_payments
		WHERE invoice_id = $1 AND status = 'failed'
		ORDER BY created_at DESC
		LIMIT 1
	`, inv.ID).Scan(&rail, &railPaymentID, &failureCode, &failureMessage))
	require.Equal(t, string(models.RailMobius), rail)
	require.Equal(t, "declined_invoice:"+inv.ID.String()+":500", railPaymentID)
	require.Equal(t, "card_declined", failureCode)
	require.Equal(t, "card declined", failureMessage)
}

func TestChargeOutstanding_WithScopedCharger_TransientErrorRetriesWithoutSettlement(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailMobius))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 500))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "scoped-transient", 500)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	adapter := &fakeCollectionAdapter{transientFailures: 1}
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailMobius): adapter,
	})
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.ErrorContains(t, err, "gateway timeout")
	require.Equal(t, 0, n)
	require.Len(t, adapter.charges, 1)

	var attempts int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		FROM openrails.invoice_payments
		WHERE invoice_id = $1
	`, inv.ID).Scan(&attempts))
	require.Equal(t, 0, attempts, "transient adapter error must not create a local payment attempt")

	n, err = svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, adapter.charges, 2)

	var settled int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		FROM openrails.invoice_payments
		WHERE invoice_id = $1 AND status = 'settled'
	`, inv.ID).Scan(&settled))
	require.Equal(t, 1, settled)
}

func TestInvoiceWorker_UsesMerchantInvoiceThresholds(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailMobius))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "worker-scoped-collection", 500)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	collectionThreshold := int64(1_000)
	monthlyFloor := int64(100)
	require.NoError(t, merchantconfig.NewStore(dbi).Upsert(ctx, models.MerchantConfiguration{
		InvoiceCollectionThreshold: &collectionThreshold,
		InvoiceMonthlyFloor:        &monthlyFloor,
		InvoiceBillingBoundary:     money.InvoiceBoundaryCalendarMonth,
	}))

	adapter := &fakeCollectionAdapter{}
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailMobius): adapter,
	})
	err = riverjobs.InvoiceWorker{Money: svc, Charger: ch}.Work(ctx, &river.Job[riverjobs.InvoiceArgs]{
		Args: riverjobs.InvoiceArgs{Collect: true},
	})
	require.NoError(t, err)
	require.Empty(t, adapter.charges, "custom collection threshold skips the smaller invoice")

	err = riverjobs.InvoiceWorker{Money: svc, Charger: ch}.Work(ctx, &river.Job[riverjobs.InvoiceArgs]{
		Args: riverjobs.InvoiceArgs{Collect: true, UseMonthlyFloor: true},
	})
	require.NoError(t, err)
	require.Len(t, adapter.charges, 1)

	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(0), paid.AmountDue)
}
