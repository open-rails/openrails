//go:build integration

package money_test

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestAutoTopupSafety_ConcurrentCapAndRollingBoundary(t *testing.T) {
	svc, dbi, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	pm := seedTopupAccount(t, ctx, svc, pool, payer, "stripe")
	clock := clockwork.NewFakeClockAt(time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC))
	svc.SetClock(clock)
	store := merchantconfig.NewStore(dbi)
	cfg, _, err := store.Get(ctx)
	require.NoError(t, err)
	old := cfg
	t.Cleanup(func() { require.NoError(t, store.Upsert(ctx, old)) })
	cfg.AutoTopupSafety = &models.AutoTopupSafetyPolicy{MaxDaily: 1, MaxWeekly: 2, MaxMonthly: 3, DeclinesBeforeDisable: 3}
	require.NoError(t, store.Upsert(ctx, cfg))

	var wg sync.WaitGroup
	admitted := make(chan money.AutoTopupEpisode, 16)
	failures := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			in := money.AutoTopupEpisode{IntentID: uuid.New(), CustomerID: payer.UUID(), PaymentMethodID: pm, Currency: currency, Anchor: "genesis", Amount: 50_000_000}
			existing, err := svc.ReserveAutoTopup(ctx, in)
			if err == nil && existing == nil {
				admitted <- in
			} else {
				failures <- err
			}
		})
	}
	wg.Wait()
	close(admitted)
	close(failures)
	require.Len(t, admitted, 1)
	for err := range failures {
		require.ErrorIs(t, err, money.ErrAutoTopupSafety)
	}
	first := <-admitted
	require.NoError(t, svc.RecordAutoTopupReceipt(ctx, first.IntentID, money.AutoTopupReceipt{TransactionID: "exact-1"}))
	_, err = svc.FinalizeAutoTopupReceipt(ctx, first)
	require.NoError(t, err)
	anchor := strconv.FormatInt(clock.Now().Unix(), 10)
	second := first
	second.IntentID = uuid.New()
	second.Anchor = anchor
	_, err = svc.ReserveAutoTopup(ctx, second)
	require.ErrorIs(t, err, money.ErrAutoTopupSafety)
	clock.Advance(24 * time.Hour)
	existing, err := svc.ReserveAutoTopup(ctx, second)
	require.NoError(t, err)
	require.Nil(t, existing)
	// Unknown keeps the one-live-episode exclusion after all rolling windows.
	clock.Advance(31 * 24 * time.Hour)
	third := second
	third.IntentID = uuid.New()
	_, err = svc.ReserveAutoTopup(ctx, third)
	require.ErrorIs(t, err, money.ErrAutoTopupSafety)
	status, err := svc.GetAutoTopupStatus(ctx, payer, currency)
	require.NoError(t, err)
	require.True(t, status.Pending)
	require.Zero(t, status.Monthly)
}

func TestAutoTopupSafety_DefinitiveDeclinesDisableExactlyOnce(t *testing.T) {
	svc, _, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	pm := seedTopupAccount(t, ctx, svc, pool, payer, "stripe")
	clock := clockwork.NewFakeClockAt(time.Date(2041, 1, 1, 0, 0, 0, 0, time.UTC))
	svc.SetClock(clock)
	anchor := "genesis"
	for i := 1; i <= 3; i++ {
		in := money.AutoTopupEpisode{IntentID: uuid.New(), CustomerID: payer.UUID(), PaymentMethodID: pm, Currency: currency, Anchor: anchor, Amount: 50_000_000}
		_, err := svc.ReserveAutoTopup(ctx, in)
		require.NoError(t, err)
		require.NoError(t, svc.RecordAutoTopupReceipt(ctx, in.IntentID, money.AutoTopupReceipt{Declined: true, Reason: "declined"}))
		for range 2 {
			receipt, err := svc.FinalizeAutoTopupReceipt(ctx, in)
			require.NoError(t, err)
			require.True(t, receipt.Declined)
		}
		status, err := svc.GetAutoTopupStatus(ctx, payer, currency)
		require.NoError(t, err)
		require.EqualValues(t, i, status.ConsecutiveDeclines)
		require.Equal(t, i < 3, status.Enabled)
		anchor = strconv.FormatInt(clock.Now().Unix(), 10)
		clock.Advance(time.Hour)
	}
	var notices int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM openrails.notification_queue WHERE customer_id=$1 AND event_type='auto_topup_disabled'", payer.UUID()).Scan(&notices))
	require.Equal(t, 1, notices)
	// Unrelated settings writes do not undo automatic disablement or counters.
	threshold := int64(500)
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{LowBalanceThreshold: &threshold})
	require.NoError(t, err)
	status, err := svc.GetAutoTopupStatus(ctx, payer, currency)
	require.NoError(t, err)
	require.False(t, status.Enabled)
	require.EqualValues(t, 3, status.ConsecutiveDeclines)
	enabled := true
	_, err = svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{AutoTopupEnabled: &enabled})
	require.NoError(t, err)
	status, err = svc.GetAutoTopupStatus(ctx, payer, currency)
	require.NoError(t, err)
	require.True(t, status.Enabled)
	require.Zero(t, status.ConsecutiveDeclines)
	require.EqualValues(t, 3, status.Daily, "explicit re-enable does not erase safety reservations")
}

func TestAutoTopupSafety_ReceiptFinalizesAfterDisableAndMethodDeletion(t *testing.T) {
	svc, _, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	pm := seedTopupAccount(t, ctx, svc, pool, payer, "stripe")
	in := money.AutoTopupEpisode{IntentID: uuid.New(), CustomerID: payer.UUID(), PaymentMethodID: pm, Currency: currency, Anchor: "genesis", Amount: 50_000_000}
	_, err := svc.ReserveAutoTopup(ctx, in)
	require.NoError(t, err)
	require.NoError(t, svc.RecordAutoTopupReceipt(ctx, in.IntentID, money.AutoTopupReceipt{TransactionID: "exact-success"}))
	disabled := false
	_, err = svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{AutoTopupEnabled: &disabled})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id=$1", pm)
	require.NoError(t, err)
	in.Amount = 99_000_000 // A refreshed intent payload cannot change the reserved charge amount.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { _, err := svc.FinalizeAutoTopupReceipt(ctx, in); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	balance, err := svc.GetBalanceForCustomer(ctx, payer, currency)
	require.NoError(t, err)
	require.EqualValues(t, 50_000_500, balance.Balance)
	status, err := svc.GetAutoTopupStatus(ctx, payer, currency)
	require.NoError(t, err)
	require.False(t, status.Enabled)
	require.False(t, status.Pending)
}

func TestAutoTopupSafety_DisabledAndCrossCustomerNeverReserve(t *testing.T) {
	svc, _, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	pm := seedTopupAccount(t, ctx, svc, pool, payer, "stripe")
	disabled := false
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{AutoTopupEnabled: &disabled})
	require.NoError(t, err)
	in := money.AutoTopupEpisode{IntentID: uuid.New(), CustomerID: payer.UUID(), PaymentMethodID: pm, Currency: currency, Anchor: "genesis", Amount: 50_000_000}
	_, err = svc.ReserveAutoTopup(ctx, in)
	require.ErrorIs(t, err, money.ErrAutoTopupSafety)
	other := identity.CustomerID(uuid.New())
	enabled := true
	amount := in.Amount
	_, err = svc.UpsertAccountSettings(ctx, other, currency, money.AccountSettingsInput{AutoTopupEnabled: &enabled, AutoTopupAmount: &amount, AutoTopupPaymentMethod: &pm})
	require.NoError(t, err)
	in.CustomerID = other.UUID()
	_, err = svc.ReserveAutoTopup(ctx, in)
	require.ErrorIs(t, err, money.ErrAutoTopupSafety)
	var n int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM openrails.auto_topup_episodes WHERE intent_id=$1", in.IntentID).Scan(&n))
	require.Zero(t, n)
}

func TestAutoTopupSafety_WeeklyMonthlyBoundaries(t *testing.T) {
	for _, window := range []struct {
		name     string
		duration time.Duration
	}{{"weekly", 7 * 24 * time.Hour}, {"monthly", 30 * 24 * time.Hour}} {
		t.Run(window.name, func(t *testing.T) {
			svc, dbi, pool, payer, currency, ctx := moneyInEnvWithDB(t)
			pm := seedTopupAccount(t, ctx, svc, pool, payer, "stripe")
			clock := clockwork.NewFakeClockAt(time.Date(2042, 1, 1, 0, 0, 0, 0, time.UTC))
			svc.SetClock(clock)
			store := merchantconfig.NewStore(dbi)
			cfg, _, err := store.Get(ctx)
			require.NoError(t, err)
			old := cfg
			t.Cleanup(func() { require.NoError(t, store.Upsert(ctx, old)) })
			policy := models.AutoTopupSafetyPolicy{MaxDaily: 100, MaxWeekly: 100, MaxMonthly: 100, DeclinesBeforeDisable: 3}
			if window.name == "weekly" {
				policy.MaxWeekly = 1
			} else {
				policy.MaxMonthly = 1
			}
			cfg.AutoTopupSafety = &policy
			require.NoError(t, store.Upsert(ctx, cfg))
			first := money.AutoTopupEpisode{IntentID: uuid.New(), CustomerID: payer.UUID(), PaymentMethodID: pm, Currency: currency, Anchor: "genesis", Amount: 50_000_000}
			_, err = svc.ReserveAutoTopup(ctx, first)
			require.NoError(t, err)
			require.NoError(t, svc.RecordAutoTopupReceipt(ctx, first.IntentID, money.AutoTopupReceipt{TransactionID: "exact-window"}))
			_, err = svc.FinalizeAutoTopupReceipt(ctx, first)
			require.NoError(t, err)
			second := first
			second.IntentID = uuid.New()
			second.Anchor = strconv.FormatInt(clock.Now().Unix(), 10)
			clock.Advance(window.duration - time.Microsecond)
			_, err = svc.ReserveAutoTopup(ctx, second)
			require.ErrorIs(t, err, money.ErrAutoTopupSafety)
			clock.Advance(time.Microsecond)
			_, err = svc.ReserveAutoTopup(ctx, second)
			require.NoError(t, err)
		})
	}
}

func TestAutoTopupSafety_CurrenciesAndMerchantReadsStayIsolated(t *testing.T) {
	svc, _, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	pm := seedTopupAccount(t, ctx, svc, pool, payer, "stripe")
	first := money.AutoTopupEpisode{IntentID: uuid.New(), CustomerID: payer.UUID(), PaymentMethodID: pm, Currency: currency, Anchor: "genesis", Amount: 50_000_000}
	_, err := svc.ReserveAutoTopup(ctx, first)
	require.NoError(t, err)
	enabled := true
	amount := int64(100)
	_, err = svc.UpsertAccountSettings(ctx, payer, "JPY", money.AccountSettingsInput{AutoTopupEnabled: &enabled, AutoTopupAmount: &amount, AutoTopupPaymentMethod: &pm})
	require.NoError(t, err)
	second := first
	second.IntentID = uuid.New()
	second.Currency = "JPY"
	second.Amount = amount
	_, err = svc.ReserveAutoTopup(ctx, second)
	require.NoError(t, err, "USD pending does not consume JPY's independently authorized cap")
	for _, cur := range []string{currency, "JPY"} {
		status, err := svc.GetAutoTopupStatus(ctx, payer, cur)
		require.NoError(t, err)
		require.EqualValues(t, 1, status.Daily)
	}
	otherCtx := merchant.WithID(ctx, merchant.ID(uuid.New()))
	episode, err := svc.GetAutoTopupEpisode(otherCtx, first.IntentID)
	require.NoError(t, err)
	require.Nil(t, episode)
}
