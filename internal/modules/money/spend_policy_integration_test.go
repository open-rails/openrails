//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

// spendTestEnv connects to the integration DB, verifies the money schema, and
// seeds a funded payer. Money has no credit_type dimension (#472); the returned
// currency is always USD.
func spendTestEnv(t *testing.T) (*money.MoneyService, *pgxpool.Pool, identity.CustomerID, string, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()

	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestTenant(ctx, t, pool)
	ctx = dbtest.WithTestTenant(ctx)

	payer := identity.CustomerIDFromString(uuid.NewString())
	require.False(t, payer.IsZero())
	payerID := payer.UUID()

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_spend_limits WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_accounts WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_balances WHERE customer_id = $1", payerID)
	})

	svc := money.NewMoneyService(dbi)
	// Fund the payer generously so balance is never the binding constraint here.
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Actor: payerID.String(), Amount: 1_000_000, Source: "test_seed",
	})
	require.NoError(t, err)
	return svc, pool, payer, money.DefaultCurrency, ctx
}

func TestSpendPolicy_DefaultsAllow(t *testing.T) {
	svc, _, payer, _, ctx := spendTestEnv(t)
	// No settings row -> prepaid defaults, no caps.
	s, err := svc.GetAccountSettings(ctx, payer)
	require.NoError(t, err)
	require.Equal(t, money.BillingModePrepaid, s.BillingMode)
	require.True(t, s.HardStopOnBreach)
	require.NotNil(t, s.DefaultCreditExpiryDays)
	require.Equal(t, 365, *s.DefaultCreditExpiryDays)

	dec, err := svc.CheckSpendAllowed(ctx, payer, "user:alice", 999_999)
	require.NoError(t, err)
	require.True(t, dec.Allowed)
	require.Empty(t, dec.DenyCode)
}

func TestSpendPolicy_DailyCap(t *testing.T) {
	svc, _, payer, currency, ctx := spendTestEnv(t)
	cap := int64(1000)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{MaxSpendPerDayMicros: &cap})
	require.NoError(t, err)

	// Spend 500 (settled withdrawal).
	_, err = svc.Withdraw(ctx, money.WithdrawParams{CustomerID: &payer, Actor: "user:a", Amount: 500, Source: "usage"})
	require.NoError(t, err)

	allow, err := svc.CheckSpendAllowed(ctx, payer, "user:a", 400) // 500+400 <= 1000
	require.NoError(t, err)
	require.True(t, allow.Allowed, "%+v", allow)

	deny, err := svc.CheckSpendAllowed(ctx, payer, "user:a", 600) // 500+600 > 1000
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, money.DenyDailyCap, deny.DenyCode)
	require.Greater(t, deny.RetryAfterSeconds, int64(0))

	// Active hold counts toward the window too (in-flight exposure).
	exp := time.Now().Add(time.Hour).UTC()
	_, err = svc.Hold(ctx, &payer, "user:a", currency, 300, "usage", "req-hold-1", exp) // window now 800
	require.NoError(t, err)
	deny2, err := svc.CheckSpendAllowed(ctx, payer, "user:a", 300) // 800+300 > 1000
	require.NoError(t, err)
	require.False(t, deny2.Allowed)
	allow2, err := svc.CheckSpendAllowed(ctx, payer, "user:a", 200) // 800+200 == 1000
	require.NoError(t, err)
	require.True(t, allow2.Allowed)
}

func TestSpendPolicy_PerActorCap(t *testing.T) {
	svc, _, payer, _, ctx := spendTestEnv(t)
	day := int64(100)
	_, err := svc.SetSpendLimit(ctx, payer, "user:alice", &day, nil)
	require.NoError(t, err)

	// alice spends 80; bob spends 80 (bob has no limit).
	_, err = svc.Withdraw(ctx, money.WithdrawParams{CustomerID: &payer, Actor: "user:alice", Amount: 80, Source: "usage"})
	require.NoError(t, err)
	_, err = svc.Withdraw(ctx, money.WithdrawParams{CustomerID: &payer, Actor: "user:bob", Amount: 80, Source: "usage"})
	require.NoError(t, err)

	deny, err := svc.CheckSpendAllowed(ctx, payer, "user:alice", 30) // 80+30 > 100
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, money.DenyActorDailyCap, deny.DenyCode)

	allow, err := svc.CheckSpendAllowed(ctx, payer, "user:alice", 20) // 80+20 == 100
	require.NoError(t, err)
	require.True(t, allow.Allowed)

	// bob has no per-actor limit and there's no org cap -> allowed even for a big estimate.
	bob, err := svc.CheckSpendAllowed(ctx, payer, "user:bob", 100_000)
	require.NoError(t, err)
	require.True(t, bob.Allowed, "%+v", bob)
}

func TestSpendPolicy_OutstandingCeiling(t *testing.T) {
	svc, _, payer, currency, ctx := spendTestEnv(t)
	ceil := int64(1000)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), MaxOutstandingOwedMicros: &ceil,
	})
	require.NoError(t, err)

	// Reserve 800 in active holds -> exposure 800.
	exp := time.Now().Add(time.Hour).UTC()
	_, err = svc.Hold(ctx, &payer, "user:a", currency, 800, "usage", "req-out-1", exp)
	require.NoError(t, err)

	deny, err := svc.CheckSpendAllowed(ctx, payer, "user:a", 300) // 800+300 > 1000
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, money.DenyOutstandingCap, deny.DenyCode)

	allow, err := svc.CheckSpendAllowed(ctx, payer, "user:a", 200) // 800+200 == 1000
	require.NoError(t, err)
	require.True(t, allow.Allowed)
}

func TestSpendPolicy_WarnOnlyDoesNotBlock(t *testing.T) {
	svc, _, payer, _, ctx := spendTestEnv(t)
	cap := int64(100)
	hard := false
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{
		MaxSpendPerDayMicros: &cap, HardStopOnBreach: &hard,
	})
	require.NoError(t, err)
	_, err = svc.Withdraw(ctx, money.WithdrawParams{CustomerID: &payer, Actor: "user:a", Amount: 100, Source: "usage"})
	require.NoError(t, err)

	dec, err := svc.CheckSpendAllowed(ctx, payer, "user:a", 50) // over cap but warn-only
	require.NoError(t, err)
	require.True(t, dec.Allowed, "warn-only should allow")
	require.Equal(t, money.DenyDailyCap, dec.DenyCode, "warn-only still reports the breach")
}

func TestSpendPolicy_SettingsRoundTrip(t *testing.T) {
	svc, _, payer, _, ctx := spendTestEnv(t)
	day, mon := int64(5000), int64(100000)
	thr := int64(2000)
	pct := 90
	out, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), MaxSpendPerDayMicros: &day,
		MaxSpendPerMonthMicros: &mon, LowBalanceThreshold: &thr, AlertThresholdPct: &pct,
	})
	require.NoError(t, err)
	require.Equal(t, money.BillingModeArrears, out.BillingMode)
	require.Equal(t, int64(5000), *out.MaxSpendPerDayMicros)
	require.Equal(t, 90, out.AlertThresholdPct)

	got, err := svc.GetAccountSettings(ctx, payer)
	require.NoError(t, err)
	require.Equal(t, int64(100000), *got.MaxSpendPerMonthMicros)
	require.Equal(t, int64(2000), *got.LowBalanceThreshold)

	// Update one field; others persist.
	day2 := int64(7000)
	_, err = svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{MaxSpendPerDayMicros: &day2})
	require.NoError(t, err)
	got2, err := svc.GetAccountSettings(ctx, payer)
	require.NoError(t, err)
	require.Equal(t, int64(7000), *got2.MaxSpendPerDayMicros)
	require.Equal(t, money.BillingModeArrears, got2.BillingMode, "unset field must persist")

	// Invalid billing mode rejected.
	_, err = svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{BillingMode: strptr("weird")})
	require.Error(t, err)
}

func strptr(s string) *string { return &s }
