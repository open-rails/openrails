//go:build integration

package credits_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

// moneyInEnv seeds a fresh credit type + payer with NO initial deposit (balance 0).
func moneyInEnv(t *testing.T) (*credits.CreditsService, *pgxpool.Pool, identity.TenantSubjectID, string, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()

	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	var hasSettings bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='credit_account_settings')").
		Scan(&hasSettings))
	if !hasSettings {
		t.Skip("billing.credit_account_settings missing; run migration 043")
	}

	now := time.Now().UTC().Truncate(time.Second)
	ctName := "test_moneyin_" + uuid.NewString()
	ctID := uuid.New()
	_, err := gen.New(pool).CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID: ctID, Name: ctName, DisplayName: "Money-in Test", Unit: "cents",
		DecimalPlaces: 2, IsActive: true, CreatedAt: now,
	})
	require.NoError(t, err)

	payer := identity.TenantSubjectIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_spend_limits WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_account_settings WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_blocks WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_transactions WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_balances WHERE tenant_subject_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.credit_types WHERE id = $1", ctID)
	})
	return credits.NewCreditsService(dbi), pool, payer, ctName, ctx
}

// --- fakes ---

type fakeCharger struct {
	charges    []credits.ChargeRequest
	declineAll bool
}

func (f *fakeCharger) ChargeSavedMethod(_ context.Context, req credits.ChargeRequest) (credits.ChargeResult, error) {
	f.charges = append(f.charges, req)
	if f.declineAll {
		return credits.ChargeResult{Declined: true}, nil
	}
	return credits.ChargeResult{TransactionID: "tx_" + req.IdempotencyKey}, nil
}

type fakeAlerter struct{ calls int }

func (f *fakeAlerter) LowBalanceAlert(_ context.Context, _ identity.TenantSubjectID, _ string, _, _ int64) error {
	f.calls++
	return nil
}

func latestBlock(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payerID uuid.UUID) *models.CreditBlock {
	t.Helper()
	b := new(models.CreditBlock)
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT expires_at FROM billing.credit_blocks WHERE tenant_subject_id = $1 ORDER BY created_at DESC LIMIT 1",
		payerID).Scan(&b.ExpiresAt))
	return b
}

// --- #240 expiry default ---

func TestDeposit_DefaultExpiry_NoSettingsRow(t *testing.T) {
	svc, pool, payer, ct, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, credits.CreditDepositParams{
		TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 1000,
		Source: "purchase", ApplyAccountExpiryDefault: true,
	})
	require.NoError(t, err)
	b := latestBlock(t, pool, ctx, payer.UUID())
	require.NotNil(t, b.ExpiresAt, "default 365d expiry should be applied")
	days := b.ExpiresAt.Sub(time.Now().UTC()).Hours() / 24
	require.InDelta(t, 365, days, 1.5)
}

func TestDeposit_NoFlag_Permanent(t *testing.T) {
	svc, pool, payer, ct, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, credits.CreditDepositParams{
		TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 1000, Source: "grant",
	})
	require.NoError(t, err)
	b := latestBlock(t, pool, ctx, payer.UUID())
	require.Nil(t, b.ExpiresAt, "no flag, no explicit expiry -> permanent")
}

func TestDeposit_ConfiguredExpiryDays(t *testing.T) {
	svc, pool, payer, ct, ctx := moneyInEnv(t)
	days := 30
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{DefaultCreditExpiryDays: &days})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{
		TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 1000,
		Source: "purchase", ApplyAccountExpiryDefault: true,
	})
	require.NoError(t, err)
	b := latestBlock(t, pool, ctx, payer.UUID())
	require.NotNil(t, b.ExpiresAt)
	require.InDelta(t, 30, b.ExpiresAt.Sub(time.Now().UTC()).Hours()/24, 1.5)
}

// --- #240 low-balance alerts ---

func TestRunLowBalanceAlerts(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	thr := int64(1000)
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{LowBalanceThreshold: &thr})
	require.NoError(t, err)
	// available 500 < 1000
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	al := &fakeAlerter{}
	n, err := svc.RunLowBalanceAlerts(ctx, al, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, al.calls)

	// Re-run within cooldown -> deduped.
	n2, err := svc.RunLowBalanceAlerts(ctx, al, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 0, n2)
	require.Equal(t, 1, al.calls)
}

// --- #239 auto-top-up ---

func TestRunAutoTopups_ChargesAndDeposits(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	thr, amt := int64(1000), int64(5000)
	pm := uuid.New()
	enabled := true
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{
		LowBalanceThreshold: &thr, AutoTopupEnabled: &enabled, AutoTopupAmountCents: &amt, AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	ch := &fakeCharger{}
	n, err := svc.RunAutoTopups(ctx, ch, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ch.charges, 1)
	// auto_topup_amount_cents charges the card in cents, as configured.
	require.Equal(t, int64(5000), ch.charges[0].AmountCents)
	require.Equal(t, pm, ch.charges[0].PaymentMethodID)

	bal, err := svc.GetBalanceForTenantSubject(ctx, payer, ct)
	require.NoError(t, err)
	// Ledger is micros: 500 seed + 5000 cents * 10_000 micros/cent deposited.
	require.Equal(t, int64(50_000_500), bal.Balance)

	// Re-run within cooldown: no second charge.
	n2, err := svc.RunAutoTopups(ctx, ch, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 0, n2)
	require.Len(t, ch.charges, 1)
}

func TestRunAutoTopups_Declined(t *testing.T) {
	svc, _, payer, ct, ctx := moneyInEnv(t)
	thr, amt := int64(1000), int64(5000)
	pm := uuid.New()
	enabled := true
	_, err := svc.UpsertAccountSettings(ctx, payer, ct, credits.AccountSettingsInput{
		LowBalanceThreshold: &thr, AutoTopupEnabled: &enabled, AutoTopupAmountCents: &amt, AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, credits.CreditDepositParams{TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 500, Source: "seed"})
	require.NoError(t, err)

	ch := &fakeCharger{declineAll: true}
	n, err := svc.RunAutoTopups(ctx, ch, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, ch.charges, 1, "charge attempted")
	bal, err := svc.GetBalanceForTenantSubject(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, int64(500), bal.Balance, "declined -> no deposit")
}
