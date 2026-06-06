//go:build integration

package credits_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/stretchr/testify/require"
)

func TestAccrueOwed_Idempotent(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{BillingMode: strptr(credits.BillingModeArrears)})
	require.NoError(t, err)

	_, err = svc.AccrueOwed(ctx, payer, ct, "usage", "req1", 300)
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, ct, "usage", "req1", 300) // duplicate
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, ct, "usage", "req2", 200)
	require.NoError(t, err)

	owed, err := svc.GetOutstandingOwed(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, int64(500), owed, "300 (idempotent) + 200")
}

func TestChargeOutstanding_Threshold(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	pm := uuid.New()
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{
		BillingMode: strptr(credits.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, ct, "usage", "r1", 500)
	require.NoError(t, err)

	ch := &fakeCharger{}
	// Below threshold first: owed 500, threshold 1000 -> no charge.
	n, err := svc.ChargeOutstanding(ctx, ch, 1000)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Empty(t, ch.charges)

	// At/over threshold: owed 500, threshold 500 -> charge.
	n, err = svc.ChargeOutstanding(ctx, ch, 500)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ch.charges, 1)
	require.Equal(t, int64(500), ch.charges[0].AmountCents)

	owed, err := svc.GetOutstandingOwed(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, int64(0), owed)
}

func TestChargeOutstanding_MonthEndSweep(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	pm := uuid.New()
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{
		BillingMode: strptr(credits.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, ct, "usage", "r1", 300)
	require.NoError(t, err)

	ch := &fakeCharger{}
	// threshold <= 0 => sweep everything with owed > 0.
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, int64(300), ch.charges[0].AmountCents)
	owed, err := svc.GetOutstandingOwed(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, int64(0), owed)
}

func TestChargeOutstanding_Declined_LeavesOwed(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	pm := uuid.New()
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{
		BillingMode: strptr(credits.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, ct, "usage", "r1", 400)
	require.NoError(t, err)

	ch := &fakeCharger{declineAll: true}
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, ch.charges, 1, "charge attempted")
	owed, err := svc.GetOutstandingOwed(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, int64(400), owed, "decline leaves owed in place")
}

func TestChargeOutstanding_NoPaymentMethod_Skipped(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{BillingMode: strptr(credits.BillingModeArrears)})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, ct, "usage", "r1", 500)
	require.NoError(t, err)

	ch := &fakeCharger{}
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Empty(t, ch.charges, "no card on file -> not charged")
}
