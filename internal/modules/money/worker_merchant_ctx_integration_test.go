//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// #673: InvoiceWorker runs from River with NO merchant in
// context — before the fix every run died in merchant.Require (ErrNoMerchant)
// and arrears were never collected. These tests run the
// workers exactly as River does (plain context.Background()) and assert real
// money effects for a seeded merchant.

func TestInvoiceWorker_NoMerchantContext_CollectsSeededMerchant(t *testing.T) {
	svc, dbi, pool, payer, cur, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetInvoiceCollectionPaymentMethod(ctx, payer, money.DefaultCurrency, pm))
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
	err = riverjobs.InvoiceWorker{DB: dbi, Money: svc, Intents: collectionRunner(dbi, ch, adapter)}.Work(context.Background(), &river.Job[riverjobs.InvoiceArgs]{
		Args: riverjobs.InvoiceArgs{Collect: true, CollectionThresholdAmount: 1},
	})
	require.NoError(t, err)

	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status, "worker with no merchant ctx must still collect the seeded merchant's invoice")
	require.Equal(t, int64(0), paid.AmountDue)
}
