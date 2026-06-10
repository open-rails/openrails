//go:build integration

package credits_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// spendTestEnv connects to the integration DB, verifies the #237 schema, and
// seeds a fresh credit type + payer. It returns the service, the tenant subject, the
// credit-type name, and a cleanup-registered context.
func spendTestEnv(t *testing.T) (*credits.CreditsService, *bun.DB, identity.TenantSubjectID, string, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)
	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	var hasSettings bool
	require.NoError(t, bunDB.NewSelect().
		ColumnExpr("EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_account_settings')").
		Scan(ctx, &hasSettings))
	if !hasSettings {
		t.Skip("billing.credit_account_settings missing; run migration 043 before integration tests")
	}

	dbi := dbtest.OpenAppDB(t, dsn)

	now := time.Now().UTC().Truncate(time.Second)
	ctName := "test_spend_" + uuid.NewString()
	ctID := uuid.New()
	_, err := bunDB.NewInsert().Model(&models.CreditType{
		ID: ctID, Name: ctName, DisplayName: "Spend Policy Test", Unit: "cents",
		DecimalPlaces: 2, IsActive: true, CreatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	payer := identity.TenantSubjectIDFromString(uuid.NewString())
	require.False(t, payer.IsZero())
	payerID := payer.UUID()

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.CreditSpendLimit)(nil)).Where("tenant_subject_id = ?", payerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditAccountSettings)(nil)).Where("tenant_subject_id = ?", payerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditBlock)(nil)).Where("tenant_subject_id = ?", payerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditTransaction)(nil)).Where("tenant_subject_id = ?", payerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditBalance)(nil)).Where("tenant_subject_id = ?", payerID).Exec(ctx)
		_, _ = bunDB.NewDelete().Model((*models.CreditType)(nil)).Where("id = ?", ctID).Exec(ctx)
	})

	svc := credits.NewCreditsService(dbi)
	// Fund the payer generously so balance is never the binding constraint here.
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{
		TenantSubjectID: &payer, Actor: payerID.String(), CreditType: ctName, Amount: 1_000_000, Source: "test_seed",
	})
	require.NoError(t, err)
	return svc, bunDB, payer, ctName, ctx
}

func TestSpendPolicy_DefaultsAllow(t *testing.T) {
	svc, _, payer, ct, ctx := spendTestEnv(t)
	// No settings row -> prepaid defaults, no caps.
	s, err := svc.GetAccountSettings(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, credits.BillingModePrepaid, s.BillingMode)
	require.True(t, s.HardStopOnBreach)
	require.NotNil(t, s.DefaultCreditExpiryDays)
	require.Equal(t, 365, *s.DefaultCreditExpiryDays)

	dec, err := svc.CheckSpendAllowed(ctx, payer, ct, "user:alice", 999_999)
	require.NoError(t, err)
	require.True(t, dec.Allowed)
	require.Empty(t, dec.DenyCode)
}

func TestSpendPolicy_DailyCap(t *testing.T) {
	svc, _, payer, ct, ctx := spendTestEnv(t)
	cap := int64(1000)
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{MaxSpendPerDayMicros: &cap})
	require.NoError(t, err)

	// Spend 500 (settled withdrawal).
	_, err = svc.Withdraw(ctx, credits.CreditWithdrawParams{TenantSubjectID: &payer, Actor: "user:a", CreditType: ct, Amount: 500, Source: "usage"})
	require.NoError(t, err)

	allow, err := svc.CheckSpendAllowed(ctx, payer, ct, "user:a", 400) // 500+400 <= 1000
	require.NoError(t, err)
	require.True(t, allow.Allowed, "%+v", allow)

	deny, err := svc.CheckSpendAllowed(ctx, payer, ct, "user:a", 600) // 500+600 > 1000
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, credits.DenyDailyCap, deny.DenyCode)
	require.Greater(t, deny.RetryAfterSeconds, int64(0))

	// Active hold counts toward the window too (in-flight exposure).
	exp := time.Now().Add(time.Hour).UTC()
	_, err = svc.Hold(ctx, &payer, "user:a", ct, 300, "usage", "req-hold-1", exp) // window now 800
	require.NoError(t, err)
	deny2, err := svc.CheckSpendAllowed(ctx, payer, ct, "user:a", 300) // 800+300 > 1000
	require.NoError(t, err)
	require.False(t, deny2.Allowed)
	allow2, err := svc.CheckSpendAllowed(ctx, payer, ct, "user:a", 200) // 800+200 == 1000
	require.NoError(t, err)
	require.True(t, allow2.Allowed)
}

func TestSpendPolicy_PerActorCap(t *testing.T) {
	svc, _, payer, ct, ctx := spendTestEnv(t)
	day := int64(100)
	_, err := svc.SetSpendLimit(ctx, payer, ct, "user:alice", &day, nil)
	require.NoError(t, err)

	// alice spends 80; bob spends 80 (bob has no limit).
	_, err = svc.Withdraw(ctx, credits.CreditWithdrawParams{TenantSubjectID: &payer, Actor: "user:alice", CreditType: ct, Amount: 80, Source: "usage"})
	require.NoError(t, err)
	_, err = svc.Withdraw(ctx, credits.CreditWithdrawParams{TenantSubjectID: &payer, Actor: "user:bob", CreditType: ct, Amount: 80, Source: "usage"})
	require.NoError(t, err)

	deny, err := svc.CheckSpendAllowed(ctx, payer, ct, "user:alice", 30) // 80+30 > 100
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, credits.DenyActorDailyCap, deny.DenyCode)

	allow, err := svc.CheckSpendAllowed(ctx, payer, ct, "user:alice", 20) // 80+20 == 100
	require.NoError(t, err)
	require.True(t, allow.Allowed)

	// bob has no per-actor limit and there's no org cap -> allowed even for a big estimate.
	bob, err := svc.CheckSpendAllowed(ctx, payer, ct, "user:bob", 100_000)
	require.NoError(t, err)
	require.True(t, bob.Allowed, "%+v", bob)
}

func TestSpendPolicy_OutstandingCeiling(t *testing.T) {
	svc, _, payer, ct, ctx := spendTestEnv(t)
	ceil := int64(1000)
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{
		BillingMode: strptr(credits.BillingModeArrears), MaxOutstandingOwedMicros: &ceil,
	})
	require.NoError(t, err)

	// Reserve 800 in active holds -> exposure 800.
	exp := time.Now().Add(time.Hour).UTC()
	_, err = svc.Hold(ctx, &payer, "user:a", ct, 800, "usage", "req-out-1", exp)
	require.NoError(t, err)

	deny, err := svc.CheckSpendAllowed(ctx, payer, ct, "user:a", 300) // 800+300 > 1000
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, credits.DenyOutstandingCap, deny.DenyCode)

	allow, err := svc.CheckSpendAllowed(ctx, payer, ct, "user:a", 200) // 800+200 == 1000
	require.NoError(t, err)
	require.True(t, allow.Allowed)
}

func TestSpendPolicy_WarnOnlyDoesNotBlock(t *testing.T) {
	svc, _, payer, ct, ctx := spendTestEnv(t)
	cap := int64(100)
	hard := false
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{
		MaxSpendPerDayMicros: &cap, HardStopOnBreach: &hard,
	})
	require.NoError(t, err)
	_, err = svc.Withdraw(ctx, credits.CreditWithdrawParams{TenantSubjectID: &payer, Actor: "user:a", CreditType: ct, Amount: 100, Source: "usage"})
	require.NoError(t, err)

	dec, err := svc.CheckSpendAllowed(ctx, payer, ct, "user:a", 50) // over cap but warn-only
	require.NoError(t, err)
	require.True(t, dec.Allowed, "warn-only should allow")
	require.Equal(t, credits.DenyDailyCap, dec.DenyCode, "warn-only still reports the breach")
}

func TestSpendPolicy_SettingsRoundTrip(t *testing.T) {
	svc, _, payer, ct, ctx := spendTestEnv(t)
	day, mon := int64(5000), int64(100000)
	thr := int64(2000)
	pct := 90
	out, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{
		BillingMode: strptr(credits.BillingModeArrears), MaxSpendPerDayMicros: &day,
		MaxSpendPerMonthMicros: &mon, LowBalanceThreshold: &thr, AlertThresholdPct: &pct,
	})
	require.NoError(t, err)
	require.Equal(t, credits.BillingModeArrears, out.BillingMode)
	require.Equal(t, int64(5000), *out.MaxSpendPerDayMicros)
	require.Equal(t, 90, out.AlertThresholdPct)

	got, err := svc.GetAccountSettings(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, int64(100000), *got.MaxSpendPerMonthMicros)
	require.Equal(t, int64(2000), *got.LowBalanceThreshold)

	// Update one field; others persist.
	day2 := int64(7000)
	_, err = svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{MaxSpendPerDayMicros: &day2})
	require.NoError(t, err)
	got2, err := svc.GetAccountSettings(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, int64(7000), *got2.MaxSpendPerDayMicros)
	require.Equal(t, credits.BillingModeArrears, got2.BillingMode, "unset field must persist")

	// Invalid billing mode rejected.
	_, err = svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{BillingMode: strptr("weird")})
	require.Error(t, err)
}

func strptr(s string) *string { return &s }
