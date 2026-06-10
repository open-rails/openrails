//go:build integration

package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

func admitEnv(t *testing.T) (*admission.Admitter, *credits.CreditsService, *admission.TierPolicyStore, identity.TenantSubjectID, string, context.Context, *abuse.BlocklistService) {
	t.Helper()
	ctx := context.Background()

	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	var hasTier bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='tier_policies')",
	).Scan(&hasTier))
	if !hasTier {
		t.Skip("billing.tier_policies missing; run migration 066")
	}

	now := time.Now().UTC().Truncate(time.Second)
	ctName := "admit_" + uuid.NewString()
	ctID := uuid.New()
	_, err := gen.New(pool).CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID: ctID, Name: ctName, DisplayName: "Admit Test", Unit: "cents", DecimalPlaces: 2, IsActive: true, CreatedAt: now,
	})
	require.NoError(t, err)

	payer := identity.TenantSubjectIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.tier_policies WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.budget_reservations WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_account_settings WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_blocks WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_transactions WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_balances WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_types WHERE id = $1", ctID)
	})

	rc, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Terminate(ctx) })
	conn, err := rc.ConnectionString(ctx)
	require.NoError(t, err)
	opt, err := redis.ParseURL(conn)
	require.NoError(t, err)
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(ctx).Err())

	cs := credits.NewCreditsService(dbi)
	store := admission.NewTierPolicyStore(dbi)
	bl := abuse.NewBlocklistService(dbi)
	bsvc := budgets.NewService(dbi)
	adm := admission.NewAdmitter(ratelimit.NewLimiter(rdb), cs, store, bl, bsvc)
	return adm, cs, store, payer, ctName, ctx, bl
}

func TestAdmit_ThroughputDeny(t *testing.T) {
	adm, _, store, payer, ct, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 2}}))

	req := admission.AdmitRequest{TenantSubjectID: payer, Actor: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, CreditType: ct, EstimateMicros: 0}
	for i := 0; i < 2; i++ {
		d, err := adm.Admit(ctx, req)
		require.NoError(t, err)
		require.True(t, d.Allowed, "admit %d", i+1)
	}
	d, err := adm.Admit(ctx, req)
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "throughput", d.BlockedBy)
	require.Equal(t, "request", d.BlockedUnit)
	require.Greater(t, d.RetryAfter, time.Duration(0))
}

func TestAdmit_MoneyDeny(t *testing.T) {
	adm, _, store, payer, ct, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}}))
	// no deposit -> balance 0, prepaid
	d, err := adm.Admit(ctx, admission.AdmitRequest{
		TenantSubjectID: payer, Actor: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, CreditType: ct, EstimateMicros: 500,
		Source: "usage", SourceID: "m1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "money", d.BlockedBy)
	require.Equal(t, credits.DenyInsufficientBalance, d.DenyCode)
}

func TestAdmit_Allow(t *testing.T) {
	adm, cs, store, payer, ct, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}}))
	_, err := cs.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 10_000, Source: "seed"})
	require.NoError(t, err)

	d, err := adm.Admit(ctx, admission.AdmitRequest{
		TenantSubjectID: payer, Actor: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1, "token": 50}, CreditType: ct, EstimateMicros: 500,
		Source: "usage", SourceID: "ok1", ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.NotNil(t, d.Hold, "allowed admit reserves a money hold")
	require.NotEmpty(t, d.Windows)
}

func TestAdmit_BlocklistDeny(t *testing.T) {
	adm, _, store, payer, ct, ctx, bl := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}}))
	fp := "fp_" + uuid.NewString()
	require.NoError(t, bl.Add(ctx, nil, "card_fingerprint", fp, "test"))

	d, err := adm.Admit(ctx, admission.AdmitRequest{
		TenantSubjectID: payer, Actor: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, CreditType: ct,
		BlockChecks: []admission.BlockCheck{{Kind: "card_fingerprint", Value: fp}},
	})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "blocked", d.BlockedBy)
	require.Equal(t, "card_fingerprint", d.BlockedUnit)
}

func TestAdmit_EndpointGating(t *testing.T) {
	adm, _, store, payer, ct, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		Windows:           []models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}},
		EntitledResources: []string{"dall-e-3"},
	}))

	d, err := adm.Admit(ctx, admission.AdmitRequest{TenantSubjectID: payer, Actor: "user:a", Tier: "free",
		Resource: "gpt-4o", Amounts: map[string]int64{"request": 1}, CreditType: ct})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "endpoint", d.BlockedBy)

	d, err = adm.Admit(ctx, admission.AdmitRequest{TenantSubjectID: payer, Actor: "user:a", Tier: "free",
		Resource: "dall-e-3", Amounts: map[string]int64{"request": 1}, CreditType: ct})
	require.NoError(t, err)
	require.True(t, d.Allowed)
}

func TestAdmit_BudgetDeny(t *testing.T) {
	adm, cs, store, payer, ct, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicyFull(ctx, payer, "free", models.ThroughputPolicy{
		Windows:       []models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 1000}},
		BudgetWindows: []models.BudgetWindowPolicy{{Key: "1h", WindowSeconds: 3600, LimitMillicents: 500}},
	}))
	_, err := cs.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 100_000, Source: "seed"})
	require.NoError(t, err)

	// First request reserves 400 of the 500/hour budget -> allowed.
	d, err := adm.Admit(ctx, admission.AdmitRequest{TenantSubjectID: payer, Actor: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, CreditType: ct, EstimateMicros: 400, Source: "usage", SourceID: "b1", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// Second request (200) would push the window to 600 > 500 -> budget deny.
	d, err = adm.Admit(ctx, admission.AdmitRequest{TenantSubjectID: payer, Actor: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, CreditType: ct, EstimateMicros: 200, Source: "usage", SourceID: "b2", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "budget", d.BlockedBy)
}

func TestAdmit_UnverifiedArrearsDeny(t *testing.T) {
	adm, cs, store, payer, ct, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 1000}}))
	bm := credits.BillingModeArrears
	_, err := cs.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{BillingMode: &bm})
	require.NoError(t, err)

	// arrears (credit line) + unverified payment method -> deny.
	d, err := adm.Admit(ctx, admission.AdmitRequest{TenantSubjectID: payer, Actor: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, CreditType: ct, EstimateMicros: 100, Source: "usage", SourceID: "u1", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "unverified", d.BlockedBy)

	// verify -> allowed (arrears with unlimited line).
	require.NoError(t, cs.SetPaymentMethodVerified(ctx, payer, ct, true))
	d, err = adm.Admit(ctx, admission.AdmitRequest{TenantSubjectID: payer, Actor: "user:a", Tier: "free", Resource: "gpt-4o",
		Amounts: map[string]int64{"request": 1}, CreditType: ct, EstimateMicros: 100, Source: "usage", SourceID: "u2", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.True(t, d.Allowed)
}

func TestAdmit_SuspendedDeny(t *testing.T) {
	adm, cs, store, payer, ct, ctx, _ := admitEnv(t)
	require.NoError(t, store.UpsertTierPolicy(ctx, payer, "free",
		[]models.ThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 100}}))
	require.NoError(t, cs.Suspend(ctx, payer, ct, "past_due"))

	d, err := adm.Admit(ctx, admission.AdmitRequest{TenantSubjectID: payer, Actor: "user:a", Tier: "free",
		Resource: "gpt-4o", Amounts: map[string]int64{"request": 1}, CreditType: ct})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "suspended", d.BlockedBy)
}
