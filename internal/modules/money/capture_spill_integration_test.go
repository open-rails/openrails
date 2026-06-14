//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// #302: a hold placed against an arrears credit line must capture even when the
// actual exceeds the prepaid balance — drawing balance first, then spilling to
// owed (previously this errored in withdrawBalanceAndBlocks).
func TestCaptureHold_ArrearsSpillsToOwed(t *testing.T) {
	svc, _, payer, cur, ctx := moneyInEnv(t)
	bm := money.BillingModeArrears
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{BillingMode: &bm})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{MerchantSubjectID: &payer, Actor: payer.UUID().String(), Amount: 300, Source: "seed"})
	require.NoError(t, err)

	res, err := svc.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
		Payer: payer, Actor: "user:a", EstimateMicros: 1000,
		Source: "req", SourceID: "h1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, res.Decision.Allowed)
	require.NotNil(t, res.Hold)

	_, err = svc.CaptureHold(ctx, res.Hold.ID, 800)
	require.NoError(t, err, "arrears capture past balance must not error")

	bal, _ := svc.GetBalanceForMerchantSubject(ctx, payer, cur)
	require.Equal(t, int64(0), bal.Balance, "balance drawn first")
	owed, _ := svc.GetOutstandingOwed(ctx, payer, cur)
	require.Equal(t, int64(500), owed, "remainder (800-300) spilled to owed")
}
