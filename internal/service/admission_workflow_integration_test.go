//go:build integration

package service_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func admissionWorkflow(t *testing.T) (*billingservice.Service, *money.MoneyService, *clockwork.FakeClock, *redis.Client, identity.CustomerID, context.Context) {
	t.Helper()
	d := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(context.Background())
	dbtest.EnsureTestMerchant(ctx, t, d.Pool())
	clock := clockwork.NewFakeClockAt(time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC))
	rdb := dbtest.NewSharedRedisClient(t)
	t.Cleanup(func() { _ = rdb.Close() })
	ms := money.NewMoneyService(d, clock)
	svc, err := billingservice.New(&app.Runtime{DB: d, RedisClient: rdb, MoneyService: ms, Clock: clock,
		EntitlementService: entitlements.NewEntitlementService(d), FXProvider: fx.NewMockProvider(nil)})
	require.NoError(t, err)
	payer := identity.CustomerID(uuid.New())
	_, err = ms.Deposit(ctx, money.DepositParams{CustomerID: &payer, Currency: "USD", Amount: 1000, Source: "workflow"})
	require.NoError(t, err)
	return svc, ms, clock, rdb, payer, ctx
}

func TestAdmissionDeadlineRecoveryAndZeroCapture(t *testing.T) {
	svc, ms, clock, rdb, payer, ctx := admissionWorkflow(t)
	in := billingservice.AdmitInput{CustomerID: payer, Invoker: "original", InvokerType: "payer", Currency: "USD", EstimatedAmount: 600, SourceID: uuid.NewString()}
	_, err := svc.Admit(ctx, in)
	require.ErrorIs(t, err, billingservice.ErrHoldDeadlineRequired)
	in.ExpiresAt = clock.Now().Add(time.Minute)
	admitted, err := svc.Admit(ctx, in)
	require.NoError(t, err)
	require.True(t, admitted.Allowed)
	bad := in
	bad.EstimatedAmount = 1001
	_, err = svc.Admit(ctx, bad)
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused)
	extended := clock.Now().Add(time.Hour)
	require.NoError(t, svc.ExtendHold(ctx, in.SourceID, extended))
	clock.Advance(2 * time.Minute)
	retry, err := svc.Admit(ctx, in)
	require.NoError(t, err)
	require.Equal(t, extended, retry.HoldExpiresAt.UTC())
	require.True(t, retry.Replayed)
	require.Equal(t, "open", retry.State)
	bal, err := ms.GetBalanceForCustomer(ctx, payer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 600, bal.HeldBalance)
	require.NoError(t, rdb.FlushDB(ctx).Err())
	first, err := svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{RequestID: in.SourceID, Amount: 400})
	require.NoError(t, err)
	require.Equal(t, openrails.CustomerID(payer), first.CustomerID)
	require.NotNil(t, first.LedgerTransferID)
	replay, err := svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{RequestID: in.SourceID, Amount: 400})
	require.NoError(t, err)
	first.Replayed = true
	require.Equal(t, first, replay)
	terminal, err := svc.Admit(ctx, in)
	require.NoError(t, err)
	require.True(t, terminal.Allowed)
	require.True(t, terminal.Replayed)
	require.Equal(t, "captured", terminal.State, "the original allowed receipt does not authorize another launch")
	bal, err = ms.GetBalanceForCustomer(ctx, payer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 600, bal.Balance)
	require.Zero(t, bal.HeldBalance)

	in.SourceID = uuid.NewString()
	in.ExpiresAt = clock.Now().Add(time.Minute)
	admitted, err = svc.Admit(ctx, in)
	require.NoError(t, err)
	require.True(t, admitted.Allowed)
	clock.Advance(2 * time.Minute)
	bal, err = ms.GetBalanceForCustomer(ctx, payer, "USD")
	require.NoError(t, err)
	require.Zero(t, bal.HeldBalance)
	expired, err := svc.Admit(ctx, in)
	require.NoError(t, err)
	require.True(t, expired.Replayed)
	require.Equal(t, "expired", expired.State)
	require.ErrorIs(t, svc.ExtendHold(ctx, in.SourceID, clock.Now().Add(time.Hour)), billingservice.ErrHoldDeadlinePassed)
	free, err := svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{RequestID: in.SourceID, Amount: 0})
	require.NoError(t, err)
	require.Nil(t, free.LedgerTransferID)
	require.Zero(t, free.Amount)
	_, err = svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{RequestID: uuid.NewString(), Amount: 10})
	require.Error(t, err, "a missing request cannot be charged from echoed identity")
}

func TestZeroEstimateStillEnforcesProspectiveRateAndDelegation(t *testing.T) {
	svc, ms, _, _, payer, ctx := admissionWorkflow(t)
	name := "rate-" + uuid.NewString()
	database := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	t.Cleanup(func() {
		_, err := database.Qx(ctx).Exec(ctx, "DELETE FROM billing.billing_policy_bindings WHERE merchant_id=$1 AND customer_id=$2", dbtest.TestMerchantID.UUID(), payer.UUID())
		require.NoError(t, err)
		_, err = database.Qx(ctx).Exec(ctx, "DELETE FROM billing.billing_policies WHERE merchant_id=$1 AND name=$2", dbtest.TestMerchantID.UUID(), name)
		require.NoError(t, err)
	})
	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{Name: name, Kind: "accrual_rate_cap", AccrualRateCapPerHour: 10_000_000}))
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{CustomerID: openrails.CustomerID(payer), PolicyName: name}))
	in := billingservice.AdmitInput{CustomerID: payer, Invoker: "user", InvokerType: "payer", Currency: "USD", SourceID: uuid.NewString(), AccrualRateDeltaPerHour: 11_000_000}
	result, err := svc.Admit(ctx, in)
	require.NoError(t, err)
	require.False(t, result.Allowed)
	require.Equal(t, admission.DenyAccrualRateCap, result.DenyCode)
	_, err = ms.RecordUsage(ctx, money.RecordUsageParams{Payer: &payer, Invoker: payer.String(), Currency: "USD", EventType: "compute", Amount: 1,
		Key: money.MustIdempotencyKey(money.UsageOperation("compute"), "rate-test", uuid.NewString())})
	require.NoError(t, err)
	in.AccrualRateDeltaPerHour = math.MaxInt64
	result, err = svc.Admit(ctx, in)
	require.NoError(t, err)
	require.False(t, result.Allowed)
	require.Equal(t, admission.DenyAccrualRateCap, result.DenyCode)
	in.AccrualRateDeltaPerHour = 0
	in.InvokerType = "delegated"
	result, err = svc.Admit(ctx, in)
	require.NoError(t, err)
	require.False(t, result.Allowed)
	require.Equal(t, admission.DenyDelegatedSpendNotAllowed, result.DenyCode)
	in.InvokerType = "payer"
	result, err = svc.Admit(ctx, in)
	require.NoError(t, err)
	require.True(t, result.Allowed)
	require.Nil(t, result.HoldExpiresAt)
}

func TestAdmissionCreditLineAndActualOverdraft(t *testing.T) {
	svc, ms, clock, _, payer, ctx := admissionWorkflow(t)
	in := billingservice.AdmitInput{CustomerID: payer, Invoker: "user", InvokerType: "payer", Currency: "USD", SourceID: uuid.NewString(), EstimatedAmount: 1500, ExpiresAt: clock.Now().Add(time.Hour)}
	result, err := svc.Admit(ctx, in)
	require.NoError(t, err)
	require.False(t, result.Allowed)
	mode := money.BillingModeArrears
	_, err = ms.UpsertAccountSettings(ctx, payer, "USD", money.AccountSettingsInput{BillingMode: &mode})
	require.NoError(t, err)
	require.NoError(t, ms.SetCreditLimit(ctx, payer, "USD", 500))
	result, err = svc.Admit(ctx, in)
	require.NoError(t, err)
	require.True(t, result.Allowed)
	_, err = svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{RequestID: in.SourceID, Amount: 1600})
	require.NoError(t, err)
	owed, err := ms.GetOutstandingOwed(ctx, payer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 600, owed)
	in.SourceID = uuid.NewString()
	in.EstimatedAmount = 1
	result, err = svc.Admit(ctx, in)
	require.NoError(t, err)
	require.False(t, result.Allowed)
	require.Equal(t, admission.DenyOutstandingCap, result.DenyCode)
}
