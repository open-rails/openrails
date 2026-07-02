//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// #673: InvoiceWorker and AutoTopupWorker run from River with NO merchant in
// context — before the fix every run died in merchant.Require (ErrNoMerchant)
// and arrears were never collected, top-ups never happened. These tests run the
// workers exactly as River does (plain context.Background()) and assert real
// money effects for a seeded merchant.

func TestInvoiceWorker_NoMerchantContext_CollectsSeededMerchant(t *testing.T) {
	svc, dbi, pool, payer, cur, ctx := moneyInEnvWithDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "worker-no-merchant-ctx", 5_000_000)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, cur, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)

	adapter := &fakeCollectionAdapter{}
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailNMI): adapter,
	})
	// EXACTLY like River: a bare background context, no merchant pinned.
	err = riverjobs.InvoiceWorker{DB: dbi, Money: svc, Charger: ch}.Work(context.Background(), &river.Job[riverjobs.InvoiceArgs]{
		Args: riverjobs.InvoiceArgs{Collect: true, CollectionThresholdAmount: 1},
	})
	require.NoError(t, err)

	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status, "worker with no merchant ctx must still collect the seeded merchant's invoice")
	require.Equal(t, int64(0), paid.AmountDue)
}

func TestAutoTopupWorker_NoMerchantContext_TopsUpSeededMerchant(t *testing.T) {
	svc, dbi, pool, payer, cur, ctx := moneyInEnvWithDB(t)
	thr, amt := int64(1000), int64(50_000_000)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	enabled := true
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		LowBalanceThreshold: &thr, AutoTopupEnabled: &enabled, AutoTopupAmountCents: &amt, AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	ch := &fakeCharger{}
	runner := &intents.Runner{
		Store:    intents.NewStore(dbi),
		Registry: intents.NewRegistry(intents.NewTopupChargeHandler(dbi, ch, nil, nil)),
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM openrails.rail_intents WHERE intent_type = 'topup_charge' AND merchant_id = $1", dbtest.TestMerchantID.UUID())
	})
	err = riverjobs.AutoTopupWorker{DB: dbi, Money: svc, Intents: runner}.Work(context.Background(), &river.Job[riverjobs.AutoTopupArgs]{})
	require.NoError(t, err)

	bal, err := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(50_000_500), bal.Balance, "worker with no merchant ctx must still top up the seeded merchant's account")
}
