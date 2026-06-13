//go:build integration

package money_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

func TestAccrueOwed_Idempotent(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)

	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "req1", 300)
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "req1", 300) // duplicate
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "req2", 200)
	require.NoError(t, err)

	owed, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(500), owed, "300 (idempotent) + 200")
}

func TestChargeOutstanding_Threshold(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	pm := uuid.New()
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "r1", 5_000_000) // $5 owed, in ledger micros
	require.NoError(t, err)

	ch := &fakeCharger{}
	// Below threshold first: owed $5, threshold $10 (both micros) -> no charge.
	n, err := svc.ChargeOutstanding(ctx, ch, 10_000_000)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Empty(t, ch.charges)

	// At/over threshold: owed $5, threshold $5 -> charge.
	n, err = svc.ChargeOutstanding(ctx, ch, 5_000_000)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ch.charges, 1)
	// Card charge is whole cents: ceil(5_000_000 micros / 10_000) = 500 cents.
	require.Equal(t, int64(500), ch.charges[0].AmountCents)

	owed, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), owed)
}

func TestChargeOutstanding_MonthEndSweep(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	pm := uuid.New()
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "r1", 3_000_001) // $3 + 1 micro owed
	require.NoError(t, err)

	ch := &fakeCharger{}
	// threshold <= 0 => sweep everything with owed > 0.
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	// Sub-cent owed rounds UP: ceil(3_000_001 micros / 10_000) = 301 cents.
	require.Equal(t, int64(301), ch.charges[0].AmountCents)
	owed, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), owed)
}

func TestChargeOutstanding_Declined_LeavesOwed(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	pm := uuid.New()
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "r1", 400)
	require.NoError(t, err)

	ch := &fakeCharger{declineAll: true}
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, ch.charges, 1, "charge attempted")
	owed, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(400), owed, "decline leaves owed in place")
}

func TestChargeOutstanding_NoPaymentMethod_Skipped(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "r1", 500)
	require.NoError(t, err)

	ch := &fakeCharger{}
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Empty(t, ch.charges, "no card on file -> not charged")
}
