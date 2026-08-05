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
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestProviderRefreshWatermarkAdvancesOnlyOnSuccessfulWindow(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(context.Background(), t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()))
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

	var pspID uuid.UUID
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		pspID = seedRefreshPSPForTest(t, ctx, dbi, merchantID, "stripe")
		res := worker.runProviderEventWindows(ctx, merchantID, reconcile.ProviderStripe, reconcile.ModeEnforce, nil, reconcile.PSPBinding{ID: pspID, Rail: "stripe"}, map[reconcile.Provider]reconcile.RailFetcher{
			reconcile.ProviderStripe: successFetcher,
		})
		require.Equal(t, 2, res.Windows)
		require.Zero(t, res.ProviderErrors)
		require.Len(t, successFetcher.calls, 2)
		require.Equal(t, successFetcher.calls[1].Until.UTC(), loadProviderRefreshWatermarkForTest(t, ctx, dbi, merchantID, pspID))
		return nil
	}))

	failingFetcher := &providerRefreshRecordingFetcher{provider: reconcile.ProviderStripe, err: errors.New("provider offline")}
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		before := loadProviderRefreshWatermarkForTest(t, ctx, dbi, merchantID, pspID)
		res := worker.runProviderEventWindows(ctx, merchantID, reconcile.ProviderStripe, reconcile.ModeEnforce, nil, reconcile.PSPBinding{ID: pspID, Rail: "stripe"}, map[reconcile.Provider]reconcile.RailFetcher{
			reconcile.ProviderStripe: failingFetcher,
		})
		require.Zero(t, res.Windows)
		require.Equal(t, 1, res.ProviderErrors)
		// or#823: a failed window leaves the watermark exactly where it was — that
		// unadvanced cursor IS the durable record that the window still needs
		// reading. The failure itself surfaces through the job's error, not through
		// a last_error column nothing read.
		require.Equal(t, before, loadProviderRefreshWatermarkForTest(t, ctx, dbi, merchantID, pspID))
		return nil
	}))
}

func TestProviderRefreshBackfillsEventsAndTerminalState(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(context.Background(), t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()))
	baseCtx := dbtest.WithTestMerchant(context.Background())
	merchantID := dbtest.TestMerchantID.UUID()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	originalPaymentID := uuid.New()
	customerID := uuid.UUID{}
	psid := "sub_refresh_" + uuid.NewString()
	var pspID uuid.UUID
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		pspID = seedRefreshPSPForTest(t, ctx, dbi, merchantID, "stripe")
		customerID = dbtest.EnsureCustomerIDPgx(ctx, t, dbi.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := dbi.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
		      VALUES ($1, $2, $2, $3, '{}'::jsonb, $4)`,
			productID, "refresh-prod-"+uuid.NewString(), "refresh-tier-"+uuid.NewString(), merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
		      VALUES ($1, $2, 999, 'USD', 720, true, $3)`, priceID, productID, merchantID)
		exec(`INSERT INTO openrails.subscriptions
		        (id, price_id, product_id, status, rail, rail_subscription_id,
		         current_period_starts_at, current_period_ends_at, started_at,
		         entitlements_spec_snapshot, customer_id, merchant_id, psp_id)
		      VALUES ($1, $2, $3, 'active', 'stripe', $4, $5, $6, $5, '{}'::jsonb, $7, $8, $9)`,
			subID, priceID, productID, psid, now.Add(-35*24*time.Hour), now.Add(-5*24*time.Hour), customerID, merchantID, pspID)
		exec(`INSERT INTO openrails.payments
		        (id, price_id, rail, transaction_id, amount, list_amount, currency, status, subscription_id, purchased_at, merchant_id, customer_id, psp_id)
		      VALUES ($1, $2, 'stripe', 'ch_original', 999, 999, 'USD', 'completed', $3, $4, $5, $6, $7)`,
			originalPaymentID, priceID, subID, now.Add(-20*24*time.Hour), merchantID, customerID, pspID)
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
			{TransactionID: "ch_missing", SubscriptionID: psid, Type: reconcile.TransactionTypeSale, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: now.Add(-48 * time.Hour)},
			{TransactionID: "re_missing", SubscriptionID: psid, Type: reconcile.TransactionTypeRefund, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: now.Add(-24 * time.Hour), Raw: rawProviderRefreshJSON(map[string]any{"charge": "ch_original"})},
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
		res := worker.runProviderEventWindows(ctx, merchantID, reconcile.ProviderStripe, reconcile.ModeEnforce, nil, reconcile.PSPBinding{ID: pspID, Rail: "stripe"}, map[reconcile.Provider]reconcile.RailFetcher{
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

func loadProviderRefreshWatermarkForTest(t *testing.T, ctx context.Context, dbi *db.DB, merchantID, pspID uuid.UUID) time.Time {
	t.Helper()
	var watermark time.Time
	err := dbi.Qx(ctx).QueryRow(ctx, `
SELECT watermark_at
  FROM openrails.rail_refresh_watermarks
 WHERE merchant_id = $1::uuid
   AND rail = 'stripe'
   AND event_domain = 'events'
   AND psp_id = $2::uuid
`, merchantID, pspID).Scan(&watermark)
	require.NoError(t, err)
	return watermark.UTC()
}

// seedRefreshPSPForTest declares one PSP on a rail — or#893 makes a pull refuse
// to run without the PSP its credentials armed from.
func seedRefreshPSPForTest(t *testing.T, ctx context.Context, dbi *db.DB, merchantID uuid.UUID, rail string) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	row, err := dbi.Gen(ctx).UpsertPSP(ctx, gen.UpsertPSPParams{
		MerchantID:     merchantID,
		Rail:           rail,
		AccountID:      rail + "-acct-" + uuid.NewString()[:8],
		LastVerifiedAt: &now,
	})
	require.NoError(t, err)
	return row.ID
}

// or#893: two PSPs on one rail must keep two watermarks. The identity key used
// to COALESCE a NULL psp_id to the nil uuid and the worker always passed nil,
// so both accounts shared ONE cursor: pulling mobius advanced it past a window
// paykings never read, and — watermark_at being an EXCLUSIVE lower bound — that
// window was skipped for paykings permanently.
func TestProviderRefreshWatermarksAreScopedPerPSP(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(context.Background(), t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()))
	baseCtx := dbtest.WithTestMerchant(context.Background())
	merchantID := dbtest.TestMerchantID.UUID()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	worker := &ProviderRefreshWorker{
		DB:              dbi,
		Clock:           clockwork.NewFakeClockAt(now),
		Window:          24 * time.Hour,
		SafetyLag:       time.Hour,
		InitialLookback: 72 * time.Hour,
		MaxWindows:      2,
	}

	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		pspA := seedRefreshPSPForTest(t, ctx, dbi, merchantID, "stripe")
		pspB := seedRefreshPSPForTest(t, ctx, dbi, merchantID, "stripe")
		require.NotEqual(t, pspA, pspB)

		fetcherA := &providerRefreshRecordingFetcher{provider: reconcile.ProviderStripe}
		resA := worker.runProviderEventWindows(ctx, merchantID, reconcile.ProviderStripe, reconcile.ModeEnforce, nil,
			reconcile.PSPBinding{ID: pspA, Rail: "stripe"},
			map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderStripe: fetcherA})
		require.Equal(t, 2, resA.Windows)

		// B has never been pulled. Its cursor must still be the initial
		// lookback, NOT the point A advanced to.
		fetcherB := &providerRefreshRecordingFetcher{provider: reconcile.ProviderStripe}
		resB := worker.runProviderEventWindows(ctx, merchantID, reconcile.ProviderStripe, reconcile.ModeEnforce, nil,
			reconcile.PSPBinding{ID: pspB, Rail: "stripe"},
			map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderStripe: fetcherB})
		require.Equal(t, 2, resB.Windows)
		require.Equal(t, fetcherA.calls[0].Since.UTC(), fetcherB.calls[0].Since.UTC(),
			"PSP B's first window must start at the initial lookback, not where PSP A's pull left off")

		wmA := loadProviderRefreshWatermarkForTest(t, ctx, dbi, merchantID, pspA)
		wmB := loadProviderRefreshWatermarkForTest(t, ctx, dbi, merchantID, pspB)
		require.Equal(t, fetcherA.calls[1].Until.UTC(), wmA)
		require.Equal(t, fetcherB.calls[1].Until.UTC(), wmB)

		var rows int
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.rail_refresh_watermarks
			  WHERE merchant_id = $1 AND rail = 'stripe' AND psp_id IN ($2, $3)`,
			merchantID, pspA, pspB).Scan(&rows))
		require.Equal(t, 2, rows, "each PSP owns its own watermark row")
		return nil
	}))
}

// or#893: the NULL/global lane is gone at the schema level too — the column is
// NOT NULL, so no writer can re-create an unattributed cursor.
func TestProviderRefreshWatermarkRejectsAnUnattributedRow(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(context.Background(), t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()))
	baseCtx := dbtest.WithTestMerchant(context.Background())
	merchantID := dbtest.TestMerchantID.UUID()

	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := dbi.Qx(ctx).Exec(ctx, `
INSERT INTO openrails.rail_refresh_watermarks (merchant_id, rail, psp_id, event_domain, watermark_at)
VALUES ($1, 'stripe', NULL, 'events', now())`, merchantID)
		require.Error(t, err)
		require.Contains(t, err.Error(), "psp_id")
		return nil
	}))
}
