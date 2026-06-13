//go:build integration

package money_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSuspensionAndVerificationState exercises the issue #299 suspension +
// payment-method-verification STATE on the credit account (no admission gating).
func TestSuspensionAndVerificationState(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)

	// A fresh account (no settings row) is neither suspended nor verified.
	suspended, err := svc.IsSuspended(ctx, payer, ct)
	require.NoError(t, err)
	require.False(t, suspended, "fresh account must not be suspended")

	verified, err := svc.IsPaymentMethodVerified(ctx, payer, ct)
	require.NoError(t, err)
	require.False(t, verified, "fresh account must not be payment-method verified")

	// Verify the payment method.
	require.NoError(t, svc.SetPaymentMethodVerified(ctx, payer, ct, true))
	verified, err = svc.IsPaymentMethodVerified(ctx, payer, ct)
	require.NoError(t, err)
	require.True(t, verified, "after SetPaymentMethodVerified(true) it must be verified")

	settings, err := svc.GetAccountSettings(ctx, payer, ct)
	require.NoError(t, err)
	require.NotNil(t, settings.VerifiedAt, "verified_at must be stamped when verified")

	// Un-verify clears the flag + timestamp.
	require.NoError(t, svc.SetPaymentMethodVerified(ctx, payer, ct, false))
	verified, err = svc.IsPaymentMethodVerified(ctx, payer, ct)
	require.NoError(t, err)
	require.False(t, verified, "after SetPaymentMethodVerified(false) it must not be verified")

	// Suspend.
	require.NoError(t, svc.Suspend(ctx, payer, ct, "fraud_review"))
	suspended, err = svc.IsSuspended(ctx, payer, ct)
	require.NoError(t, err)
	require.True(t, suspended, "after Suspend it must be suspended")

	settings, err = svc.GetAccountSettings(ctx, payer, ct)
	require.NoError(t, err)
	require.NotNil(t, settings.SuspendedAt, "suspended_at must be stamped")
	require.NotNil(t, settings.SuspendReason)
	require.Equal(t, "fraud_review", *settings.SuspendReason)

	// Resume clears suspension.
	require.NoError(t, svc.Resume(ctx, payer, ct))
	suspended, err = svc.IsSuspended(ctx, payer, ct)
	require.NoError(t, err)
	require.False(t, suspended, "after Resume it must not be suspended")

	settings, err = svc.GetAccountSettings(ctx, payer, ct)
	require.NoError(t, err)
	require.Nil(t, settings.SuspendedAt, "suspended_at must be cleared on resume")
	require.Nil(t, settings.SuspendReason, "suspend_reason must be cleared on resume")
}
