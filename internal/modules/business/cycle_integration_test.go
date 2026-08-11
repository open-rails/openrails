//go:build integration

package business_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/business"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

// or#910: the dunning ladder escalates notify-only notices on the existing
// notifications feed, its last rung is a suspension RECOMMENDATION signal
// (host_lifecycle_events — hosts enforce, OpenRails never revokes access),
// settlement clears the episode, and budget alerts fire once per (period,
// threshold). Every leg re-runs to prove the pass is idempotent.

func cycleEnv(t *testing.T) (*business.Service, *money.MoneyService, *db.DB, *pgxpool.Pool, identity.CustomerID, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		for _, stmt := range []string{
			"DELETE FROM openrails.notification_queue WHERE customer_id = $1",
			"DELETE FROM openrails.host_lifecycle_events WHERE subject_id = $1",
			"DELETE FROM openrails.invoice_payments WHERE customer_id = $1",
			"DELETE FROM openrails.invoice_items WHERE customer_id = $1",
			"DELETE FROM openrails.invoices WHERE customer_id = $1",
			"DELETE FROM openrails.ledger_transfers WHERE customer_id = $1",
			"DELETE FROM openrails.customer_business_profiles WHERE customer_id = $1",
			"DELETE FROM openrails.customer_invoice_profiles WHERE customer_id = $1",
			"DELETE FROM openrails.money_settings WHERE customer_id = $1",
		} {
			_, _ = pool.Exec(ctx, stmt, payerID)
		}
	})
	return business.NewService(dbi, nil), money.NewMoneyService(dbi), dbi, pool, payer, ctx
}

func notificationCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, payer identity.CustomerID, eventType string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.notification_queue WHERE customer_id = $1 AND event_type = $2",
		payer.UUID(), eventType).Scan(&n))
	return n
}

func lifecycleEventCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, payer identity.CustomerID, eventType string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.host_lifecycle_events WHERE subject_id = $1 AND event_type = $2",
		payer.UUID(), eventType).Scan(&n))
	return n
}

func TestBusinessCycle_DunningLadderToRecommendationAndBack(t *testing.T) {
	svc, ms, _, pool, payer, ctx := cycleEnv(t)
	now := time.Now().UTC()

	_, err := ms.OnboardBusinessCustomer(ctx, payer, money.BusinessOnboarding{
		TermsVersion: "2026-08", TermsAcceptedAt: now, Currency: money.DefaultCurrency,
		// send_invoice: manual remittance, so the collection plane never runs.
		InvoiceProfile: money.CustomerInvoiceProfile{CollectionMethod: money.CollectionSendInvoice},
		CreditLimit:    10_000_000,
	})
	require.NoError(t, err)

	// A receivable due immediately (net-0): accrue, then finalize the period.
	_, err = ms.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "or910-debt", 500_000)
	require.NoError(t, err)
	inv, err := ms.FinalizeInvoice(ctx, payer, money.DefaultCurrency, now.Add(-time.Hour), now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, int64(500_000), inv.AmountDue)

	// Day 1: issued + overdue notices, one each. No final notice, no episode.
	day1 := now.Add(25 * time.Hour)
	_, err = svc.EvaluateMerchant(ctx, day1)
	require.NoError(t, err)
	require.Equal(t, 1, notificationCount(t, ctx, pool, payer, "invoice_issued"))
	require.Equal(t, 1, notificationCount(t, ctx, pool, payer, "invoice_overdue"))
	require.Equal(t, 0, notificationCount(t, ctx, pool, payer, "invoice_final_notice"))

	// Re-run: nothing new — the ladder dedupes per invoice per rung.
	_, err = svc.EvaluateMerchant(ctx, day1.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, notificationCount(t, ctx, pool, payer, "invoice_issued"))
	require.Equal(t, 1, notificationCount(t, ctx, pool, payer, "invoice_overdue"))

	// Day 8: final notice.
	_, err = svc.EvaluateMerchant(ctx, now.Add(8*24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, notificationCount(t, ctx, pool, payer, "invoice_final_notice"))
	p, err := ms.GetBusinessProfile(ctx, payer)
	require.NoError(t, err)
	require.Nil(t, p.SuspensionRecommendedAt, "final notice must not open the episode yet")

	// Day 15: suspension RECOMMENDED — profile watermark, durable host signal,
	// payer notification. Access is never revoked by OpenRails.
	day15 := now.Add(15 * 24 * time.Hour)
	_, err = svc.EvaluateMerchant(ctx, day15)
	require.NoError(t, err)
	p, err = ms.GetBusinessProfile(ctx, payer)
	require.NoError(t, err)
	require.NotNil(t, p.SuspensionRecommendedAt)
	require.Contains(t, p.SuspensionReason, "past due")
	require.Equal(t, 1, lifecycleEventCount(t, ctx, pool, payer, business.EventSuspensionRecommended))
	require.Equal(t, 1, notificationCount(t, ctx, pool, payer, "business_suspension_recommended"))

	// Re-run: the episode is open, the edge cannot fire twice.
	_, err = svc.EvaluateMerchant(ctx, day15.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, lifecycleEventCount(t, ctx, pool, payer, business.EventSuspensionRecommended))
	require.Equal(t, 1, notificationCount(t, ctx, pool, payer, "business_suspension_recommended"))

	// Settle by wire, then run: the episode clears, in both channels.
	_, err = ms.RecordOutOfBandInvoicePayment(ctx, payer, inv.ID, 500_000, "wire-or910")
	require.NoError(t, err)
	_, err = svc.EvaluateMerchant(ctx, now.Add(16*24*time.Hour))
	require.NoError(t, err)
	p, err = ms.GetBusinessProfile(ctx, payer)
	require.NoError(t, err)
	require.Nil(t, p.SuspensionRecommendedAt, "settled book must clear the recommendation")
	require.Empty(t, p.SuspensionReason)
	require.Equal(t, 1, lifecycleEventCount(t, ctx, pool, payer, business.EventSuspensionCleared))
	require.Equal(t, 1, notificationCount(t, ctx, pool, payer, "business_suspension_cleared"))

	// And once cleared, another pass changes nothing.
	_, err = svc.EvaluateMerchant(ctx, now.Add(17*24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, lifecycleEventCount(t, ctx, pool, payer, business.EventSuspensionCleared))
}

func TestBusinessCycle_BudgetAlertsOncePerPeriodThreshold(t *testing.T) {
	svc, ms, _, pool, payer, ctx := cycleEnv(t)
	now := time.Now().UTC()

	_, err := ms.OnboardBusinessCustomer(ctx, payer, money.BusinessOnboarding{
		TermsVersion: "2026-08", TermsAcceptedAt: now, Currency: money.DefaultCurrency,
		BudgetAlertThresholds: []int64{100_000, 1_000_000},
		InvoiceProfile:        money.CustomerInvoiceProfile{CollectionMethod: money.CollectionSendInvoice},
	})
	require.NoError(t, err)

	// 150k of accrued-but-uninvoiced spend: crosses the first threshold only.
	_, err = ms.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "or910-budget", 150_000)
	require.NoError(t, err)

	res, err := svc.EvaluateMerchant(ctx, now)
	require.NoError(t, err)
	require.Equal(t, 1, res.BudgetAlerts)
	require.Equal(t, 1, notificationCount(t, ctx, pool, payer, "budget_alert"))

	// Re-run in the same period: no duplicate. Alerts notify, never cap —
	// nothing else about the account changed.
	res, err = svc.EvaluateMerchant(ctx, now.Add(time.Hour))
	require.NoError(t, err)
	require.Zero(t, res.BudgetAlerts)
	require.Equal(t, 1, notificationCount(t, ctx, pool, payer, "budget_alert"))

	// More spend crosses the second threshold: exactly one more alert.
	_, err = ms.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "or910-budget-2", 900_000)
	require.NoError(t, err)
	res, err = svc.EvaluateMerchant(ctx, now.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, res.BudgetAlerts)
	require.Equal(t, 2, notificationCount(t, ctx, pool, payer, "budget_alert"))
}
