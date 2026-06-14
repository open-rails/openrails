//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

// moneyInEnv provisions a fresh payer with NO initial deposit (balance 0). Money
// has no credit_type dimension (#472); the returned currency is always USD.
func moneyInEnv(t *testing.T) (*money.MoneyService, *pgxpool.Pool, identity.CustomerID, string, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()

	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestTenant(ctx, t, pool)
	ctx = dbtest.WithTestTenant(ctx)

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_spend_limits WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_blocks WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_transactions WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_balances WHERE customer_id = $1", payerID)
	})
	return money.NewMoneyService(dbi), pool, payer, money.DefaultCurrency, ctx
}

// --- fakes ---

type fakeCharger struct {
	charges    []money.ChargeRequest
	declineAll bool
}

func (f *fakeCharger) ChargeSavedMethod(_ context.Context, req money.ChargeRequest) (money.ChargeResult, error) {
	f.charges = append(f.charges, req)
	if f.declineAll {
		return money.ChargeResult{Declined: true}, nil
	}
	return money.ChargeResult{TransactionID: "tx_" + req.IdempotencyKey}, nil
}

type fakeAlerter struct{ calls int }

func (f *fakeAlerter) LowBalanceAlert(_ context.Context, _ identity.CustomerID, _, _ int64) error {
	f.calls++
	return nil
}

func latestBlock(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payerID uuid.UUID) *models.MoneyBlock {
	t.Helper()
	b := new(models.MoneyBlock)
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT expires_at FROM openrails.money_blocks WHERE customer_id = $1 ORDER BY created_at DESC LIMIT 1",
		payerID).Scan(&b.ExpiresAt))
	return b
}

// --- #240 expiry default ---

func TestDeposit_DefaultExpiry_NoSettingsRow(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Actor: payer.UUID().String(), Amount: 1000,
		Source: "purchase", ApplyAccountExpiryDefault: true,
	})
	require.NoError(t, err)
	b := latestBlock(t, pool, ctx, payer.UUID())
	require.NotNil(t, b.ExpiresAt, "default 365d expiry should be applied")
	days := b.ExpiresAt.Sub(time.Now().UTC()).Hours() / 24
	require.InDelta(t, 365, days, 1.5)
}

func TestDeposit_NoFlag_Permanent(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Actor: payer.UUID().String(), Amount: 1000, Source: "grant",
	})
	require.NoError(t, err)
	b := latestBlock(t, pool, ctx, payer.UUID())
	require.Nil(t, b.ExpiresAt, "no flag, no explicit expiry -> permanent")
}

func TestDeposit_ConfiguredExpiryDays(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	days := 30
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{DefaultCreditExpiryDays: &days})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Actor: payer.UUID().String(), Amount: 1000,
		Source: "purchase", ApplyAccountExpiryDefault: true,
	})
	require.NoError(t, err)
	b := latestBlock(t, pool, ctx, payer.UUID())
	require.NotNil(t, b.ExpiresAt)
	require.InDelta(t, 30, b.ExpiresAt.Sub(time.Now().UTC()).Hours()/24, 1.5)
}

// --- #240 low-balance alerts ---

func TestRunLowBalanceAlerts(t *testing.T) {
	svc, _, payer, _, ctx := moneyInEnv(t)
	thr := int64(1000)
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{LowBalanceThreshold: &thr})
	require.NoError(t, err)
	// available 500 < 1000
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Actor: payer.UUID().String(), Amount: 500, Source: "seed"})
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
	svc, _, payer, currency, ctx := moneyInEnv(t)
	thr, amt := int64(1000), int64(5000)
	pm := uuid.New()
	enabled := true
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{
		LowBalanceThreshold: &thr, AutoTopupEnabled: &enabled, AutoTopupAmountCents: &amt, AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Actor: payer.UUID().String(), Amount: 500, Source: "seed"})
	require.NoError(t, err)

	ch := &fakeCharger{}
	n, err := svc.RunAutoTopups(ctx, ch, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ch.charges, 1)
	// auto_topup_amount_cents charges the card in cents, as configured.
	require.Equal(t, int64(5000), ch.charges[0].AmountCents)
	require.Equal(t, pm, ch.charges[0].PaymentMethodID)

	bal, err := svc.GetBalanceForCustomer(ctx, payer, currency)
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
	svc, _, payer, currency, ctx := moneyInEnv(t)
	thr, amt := int64(1000), int64(5000)
	pm := uuid.New()
	enabled := true
	_, err := svc.UpsertAccountSettings(ctx, payer, money.AccountSettingsInput{
		LowBalanceThreshold: &thr, AutoTopupEnabled: &enabled, AutoTopupAmountCents: &amt, AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Actor: payer.UUID().String(), Amount: 500, Source: "seed"})
	require.NoError(t, err)

	ch := &fakeCharger{declineAll: true}
	n, err := svc.RunAutoTopups(ctx, ch, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, ch.charges, 1, "charge attempted")
	bal, err := svc.GetBalanceForCustomer(ctx, payer, currency)
	require.NoError(t, err)
	require.Equal(t, int64(500), bal.Balance, "declined -> no deposit")
}
