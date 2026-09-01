//go:build integration

package service_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func testMerchantKey() string { return dbtest.TestMerchantID.UUID().String() }

// xs-007 row 33: an admission hold lives exactly as long as its owner
// declared. There is no default lifetime; a running job extends its own.
func TestAdmitHold_LifetimeIsTheCallerDeclaredDeadline(t *testing.T) {
	svc, _, rdb, payer, ctx := captureFallbackEnv(t)

	// 1) A hold with no declared deadline is refused before any gate runs.
	_, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:z", InvokerType: "payer", Currency: money.DefaultCurrency,
		EstimatedAmount: 500, Source: "usage", SourceID: uuid.NewString(),
	})
	require.ErrorIs(t, err, billingservice.ErrHoldDeadlineRequired)

	// A check-only admit (no hold) still needs no deadline.
	checkOnly, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:z", InvokerType: "payer", Currency: money.DefaultCurrency,
		EstimatedAmount: 0, Source: "usage", SourceID: uuid.NewString(),
	})
	require.NoError(t, err)
	require.True(t, checkOnly.Allowed)
	require.Nil(t, checkOnly.HoldExpiresAt)

	// 2) The hold expires at the declared deadline — and at nothing else.
	reqID := uuid.NewString()
	deadline := time.Now().Add(2 * time.Second)
	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:z", InvokerType: "payer", Currency: money.DefaultCurrency,
		EstimatedAmount: 500, Source: "usage", SourceID: reqID, ExpiresAtUnix: deadline.Unix(),
	})
	require.NoError(t, err)
	require.True(t, res.Allowed)
	require.NotNil(t, res.HoldExpiresAt)
	require.WithinDuration(t, deadline, *res.HoldExpiresAt, time.Second)

	holdKeys := func() int64 {
		n, err := rdb.Exists(ctx, "sg:req:"+testMerchantKey()+":"+reqID).Result()
		require.NoError(t, err)
		return n
	}
	require.EqualValues(t, 1, holdKeys(), "hold pointer is live inside the declared window")

	// 3) The still-running job re-declares: the hold outlives its first estimate.
	require.NoError(t, svc.ExtendHold(ctx, reqID, time.Now().Add(time.Hour)))
	time.Sleep(2500 * time.Millisecond)
	require.EqualValues(t, 1, holdKeys(), "an extended hold survives its original deadline")

	// 4) Settling ends it; extending after that is refused, never resurrected.
	require.NoError(t, svc.ReleaseHold(ctx, reqID))
	require.EqualValues(t, 0, holdKeys())
	require.ErrorIs(t, svc.ExtendHold(ctx, reqID, time.Now().Add(time.Hour)), billingservice.ErrHoldNotFound)

	// 5) A deadline in the past is refused on both paths.
	_, err = svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:z", InvokerType: "payer", Currency: money.DefaultCurrency,
		EstimatedAmount: 500, Source: "usage", SourceID: uuid.NewString(), ExpiresAtUnix: time.Now().Add(-time.Minute).Unix(),
	})
	require.ErrorIs(t, err, billingservice.ErrHoldDeadlinePassed)
	require.ErrorIs(t, svc.ExtendHold(ctx, uuid.NewString(), time.Now().Add(-time.Minute)), billingservice.ErrHoldDeadlinePassed)
}

// TestAdmitHold_AbandonedHoldLapsesAtItsDeadline: a hold whose owner never
// came back frees its reservation once ITS deadline passes — the abandonment
// signal is the owner's own declaration, not a package constant.
func TestAdmitHold_AbandonedHoldLapsesAtItsDeadline(t *testing.T) {
	svc, _, rdb, payer, ctx := captureFallbackEnv(t)
	reqID := uuid.NewString()
	res, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:z", InvokerType: "payer", Currency: money.DefaultCurrency,
		EstimatedAmount: 500, Source: "usage", SourceID: reqID, ExpiresAtUnix: time.Now().Add(time.Second).Unix(),
	})
	require.NoError(t, err)
	require.True(t, res.Allowed)

	time.Sleep(2100 * time.Millisecond)
	n, err := rdb.Exists(ctx, "sg:req:"+testMerchantKey()+":"+reqID).Result()
	require.NoError(t, err)
	require.EqualValues(t, 0, n, "the abandoned hold lapsed at its declared deadline")
	require.ErrorIs(t, svc.ExtendHold(ctx, reqID, time.Now().Add(time.Hour)), billingservice.ErrHoldNotFound)
}
