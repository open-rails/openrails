//go:build integration

package credits_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/stretchr/testify/require"
)

// #302: a hold placed against an arrears credit line must capture even when the
// actual exceeds the prepaid balance — drawing balance first, then spilling to
// owed (previously this errored in withdrawBalanceAndBlocks).
func TestCaptureHold_ArrearsSpillsToOwed(t *testing.T) {
	svc, _, owner, ct, ctx := moneyInEnv(t)
	bm := credits.BillingModeArrears
	_, err := svc.UpsertAccountSettings(ctx, owner, ct, credits.AccountSettingsInput{BillingMode: &bm})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{OwnerID: &owner, UserID: owner.UUID().String(), CreditType: ct, Amount: 300, Source: "seed"})
	require.NoError(t, err)

	res, err := svc.AuthorizeAndHold(ctx, credits.AuthorizeHoldInput{
		Owner: owner, Invoker: "user:a", CreditType: ct, EstimateCents: 1000,
		Source: "req", SourceID: "h1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, res.Decision.Allowed)
	require.NotNil(t, res.Hold)

	_, err = svc.CaptureHold(ctx, res.Hold.ID, 800)
	require.NoError(t, err, "arrears capture past balance must not error")

	bal, _ := svc.GetBalanceForOwner(ctx, owner, ct)
	require.Equal(t, int64(0), bal.Balance, "balance drawn first")
	owed, _ := svc.GetOutstandingOwed(ctx, owner, ct)
	require.Equal(t, int64(500), owed, "remainder (800-300) spilled to owed")
}
