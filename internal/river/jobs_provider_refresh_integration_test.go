//go:build integration

package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestProviderRefreshWatermarkAdvancesOnlyOnSuccessfulWindow(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(context.Background(), t, dbi.Pool())
	baseCtx := dbtest.WithTestMerchant(context.Background())
	merchantID := dbtest.TestMerchantID.UUID()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(now)

	successFetcher := &providerRefreshRecordingFetcher{provider: reconcile.ProviderStripe}
	worker := &ProviderRefreshWorker{
		DB:              dbi,
		Clock:           clock,
		Window:          24 * time.Hour,
		SafetyLag:       time.Hour,
		InitialLookback: 72 * time.Hour,
		MaxWindows:      2,
	}

	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res := worker.runProviderEventWindows(ctx, merchantID, reconcile.ProviderStripe, reconcile.ModeEnforce, nil, nil, nil, map[reconcile.Provider]reconcile.RailFetcher{
			reconcile.ProviderStripe: successFetcher,
		})
		require.Equal(t, 2, res.Windows)
		require.Zero(t, res.ProviderErrors)
		require.Len(t, successFetcher.calls, 2)
		watermark, lastErr := loadProviderRefreshWatermarkForTest(t, ctx, dbi, merchantID)
		require.Equal(t, successFetcher.calls[1].Until.UTC(), watermark)
		require.Nil(t, lastErr)
		return nil
	}))

	failingFetcher := &providerRefreshRecordingFetcher{provider: reconcile.ProviderStripe, err: errors.New("provider offline")}
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		before, _ := loadProviderRefreshWatermarkForTest(t, ctx, dbi, merchantID)
		res := worker.runProviderEventWindows(ctx, merchantID, reconcile.ProviderStripe, reconcile.ModeEnforce, nil, nil, nil, map[reconcile.Provider]reconcile.RailFetcher{
			reconcile.ProviderStripe: failingFetcher,
		})
		require.Zero(t, res.Windows)
		require.Equal(t, 1, res.ProviderErrors)
		after, lastErr := loadProviderRefreshWatermarkForTest(t, ctx, dbi, merchantID)
		require.Equal(t, before, after)
		require.NotNil(t, lastErr)
		require.Contains(t, *lastErr, "provider offline")
		return nil
	}))
}

func TestProviderRefreshBackfillsEventsAndTerminalState(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(context.Background(), t, dbi.Pool())
	baseCtx := dbtest.WithTestMerchant(context.Background())
	merchantID := dbtest.TestMerchantID.UUID()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	originalPaymentID := uuid.New()
	customerID := uuid.UUID{}
	psid := "sub_refresh_" + uuid.NewString()
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customerID = dbtest.EnsureCustomerIDPgx(ctx, t, dbi.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := dbi.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
		      VALUES ($1, $2, $2, $3, '{}'::jsonb, $4)`,
			productID, "refresh-prod-"+uuid.NewString(), "refresh-tier-"+uuid.NewString(), merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
		      VALUES ($1, $2, 999, 'usd', 720, true, $3)`, priceID, productID, merchantID)
		exec(`INSERT INTO openrails.subscriptions
		        (id, price_id, product_id, status, rail, rail_subscription_id,
		         current_period_starts_at, current_period_ends_at, started_at,
		         entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1, $2, $3, 'active', 'stripe', $4, $5, $6, $5, '{}'::jsonb, $7, $8)`,
			subID, priceID, productID, psid, now.Add(-35*24*time.Hour), now.Add(-5*24*time.Hour), customerID, merchantID)
		exec(`INSERT INTO openrails.payments
		        (id, price_id, rail, transaction_id, amount, list_amount, currency, status, subscription_id, purchased_at, merchant_id, customer_id)
		      VALUES ($1, $2, 'stripe', 'ch_original', 999, 999, 'usd', 'completed', $3, $4, $5, $6)`,
			originalPaymentID, priceID, subID, now.Add(-20*24*time.Hour), merchantID, customerID)
		return nil
	}))

	snap := &reconcile.RemoteSnapshot{
		Provider:  reconcile.ProviderStripe,
		FetchedAt: now,
		Subscriptions: []reconcile.RemoteSubscription{{
			RailSubscriptionID: psid,
			Status:             reconcile.SubscriptionStatusCancelled,
			RawStatus:          "canceled",
		}},
		Transactions: []reconcile.RemoteTransaction{
			{TransactionID: "ch_missing", SubscriptionID: psid, Type: reconcile.TransactionTypeSale, Success: true, AmountCents: 999, Currency: "usd", OccurredAt: now.Add(-48 * time.Hour)},
			{TransactionID: "re_missing", SubscriptionID: psid, Type: reconcile.TransactionTypeRefund, Success: true, AmountCents: 999, Currency: "usd", OccurredAt: now.Add(-24 * time.Hour), Raw: rawProviderRefreshJSON(map[string]any{"charge": "ch_original"})},
		},
		Capabilities: reconcile.Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Chargebacks: true},
		Coverage: reconcile.SnapshotCoverage{
			SubscriptionsExhaustive:       true,
			TransactionsExhaustive:        true,
			TransactionsPaginatedComplete: true,
		},
	}
	worker := &ProviderRefreshWorker{
		DB:              dbi,
		Clock:           clockwork.NewFakeClockAt(now),
		Window:          24 * time.Hour,
		SafetyLag:       time.Hour,
		InitialLookback: 72 * time.Hour,
		MaxWindows:      1,
	}

	require.NoError(t, dbi.RunInMerchantConn(merchant.WithID(context.Background(), merchant.ID(merchantID)), func(ctx context.Context) error {
		res := worker.runProviderEventWindows(ctx, merchantID, reconcile.ProviderStripe, reconcile.ModeEnforce, nil, nil, nil, map[reconcile.Provider]reconcile.RailFetcher{
			reconcile.ProviderStripe: &providerRefreshSnapshotFetcher{provider: reconcile.ProviderStripe, snap: snap},
		})
		require.Equal(t, 1, res.Windows)
		require.Positive(t, res.AppliedChanges)
		return nil
	}))

	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var status string
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&status))
		require.Equal(t, "cancelled", status)

		var missingCharge, missingRefund int
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE transaction_id = 'ch_missing'`).Scan(&missingCharge))
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE transaction_id = 're_missing' AND refunded_payment_id = $1`, originalPaymentID).Scan(&missingRefund))
		require.Equal(t, 1, missingCharge)
		require.Equal(t, 1, missingRefund)
		return nil
	}))
}

type providerRefreshRecordingFetcher struct {
	provider reconcile.Provider
	err      error
	calls    []reconcile.FetchParams
}

type providerRefreshSnapshotFetcher struct {
	provider reconcile.Provider
	snap     *reconcile.RemoteSnapshot
}

func (f *providerRefreshSnapshotFetcher) Name() string { return string(f.provider) }

func (f *providerRefreshSnapshotFetcher) Capabilities() reconcile.Capabilities {
	return f.snap.Capabilities
}

func (f *providerRefreshSnapshotFetcher) Fetch(_ context.Context, params reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	snap := *f.snap
	since, until := params.Since.UTC(), params.Until.UTC()
	snap.Coverage.TransactionWindowSince = &since
	snap.Coverage.TransactionWindowUntil = &until
	return &snap, nil
}

func rawProviderRefreshJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (f *providerRefreshRecordingFetcher) Name() string { return string(f.provider) }

func (f *providerRefreshRecordingFetcher) Capabilities() reconcile.Capabilities {
	return reconcile.Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Chargebacks: true, Vault: true}
}

func (f *providerRefreshRecordingFetcher) Fetch(_ context.Context, params reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	f.calls = append(f.calls, params)
	if f.err != nil {
		return nil, f.err
	}
	since, until := params.Since.UTC(), params.Until.UTC()
	return &reconcile.RemoteSnapshot{
		Provider:     f.provider,
		FetchedAt:    until,
		Capabilities: f.Capabilities(),
		Coverage: reconcile.SnapshotCoverage{
			SubscriptionsExhaustive:       true,
			TransactionsExhaustive:        true,
			TransactionsPaginatedComplete: true,
			TransactionWindowSince:        &since,
			TransactionWindowUntil:        &until,
		},
	}, nil
}

func loadProviderRefreshWatermarkForTest(t *testing.T, ctx context.Context, dbi *db.DB, merchantID uuid.UUID) (time.Time, *string) {
	t.Helper()
	var watermark time.Time
	var lastErr *string
	err := dbi.Qx(ctx).QueryRow(ctx, `
SELECT watermark_at, last_error
  FROM openrails.rail_refresh_watermarks
 WHERE merchant_id = $1::uuid
   AND rail = 'stripe'
   AND event_domain = 'events'
   AND psp_id IS NULL
`, merchantID).Scan(&watermark, &lastErr)
	require.NoError(t, err)
	return watermark.UTC(), lastErr
}
