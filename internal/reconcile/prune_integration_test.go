//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #511 pull-provider --prune: an account-bound local subscription ABSENT from the
// provider snapshot is excess. A bare excess sub is deleted (with its checkout
// sessions + entitlements); an excess sub that fed the #514 grant ledger is
// SKIPPED (retracted via convergence, never row-deleted). Dry-run writes nothing.
func TestPruneProviderAccountExcess(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	suffix := uuid.NewString()[:8]
	paID := uuid.New()
	subKeep, subGone, subGrant := uuid.New(), uuid.New(), uuid.New()
	prodKeep, prodGone, prodGrant := uuid.New(), uuid.New(), uuid.New()
	priceKeep, priceGone, priceGrant := uuid.New(), uuid.New(), uuid.New()
	var customer uuid.UUID

	psubKeep := "psub-keep-" + suffix
	psubGone := "psub-gone-" + suffix
	psubGrant := "psub-grant-" + suffix

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.provider_accounts (id, merchant_id, provider_type, account_id, role, status) VALUES ($1,$2,'nmi',$3,'secondary','enabled')`,
			paID, merchantID, "acct-"+suffix)
		// One product+price per sub (uq_subscriptions_customer_product_lifecycle
		// forbids multiple live subs per customer/product).
		sub := func(id, prodID, priceID uuid.UUID, tag, psub string) {
			exec(`INSERT INTO openrails.products (id, slug, display_name, tier_group, entitlements_spec, merchant_id) VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`,
				prodID, "prune-"+tag+"-"+suffix, "prune-tier-"+tag+"-"+suffix, merchantID)
			exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, billing_cycle_days, merchant_id) VALUES ($1,$2,999,'usd',30,$3)`, priceID, prodID, merchantID)
			exec(`INSERT INTO openrails.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, provider_account_id, current_period_starts_at, current_period_ends_at, started_at, entitlements_spec_snapshot, customer_id, merchant_id)
			      VALUES ($1,$2,$3,'active','nmi',$4,$5,$6,$7,$6,'{}'::jsonb,$8,$9)`,
				id, priceID, prodID, psub, paID, time.Now().Add(-20*24*time.Hour), time.Now().Add(10*24*time.Hour), customer, merchantID)
		}
		sub(subKeep, prodKeep, priceKeep, "keep", psubKeep)
		sub(subGone, prodGone, priceGone, "gone", psubGone)
		sub(subGrant, prodGrant, priceGrant, "grant", psubGrant)
		// subGrant fed the grant ledger -> entangled -> must be skipped by prune.
		exec(`INSERT INTO openrails.grants (merchant_id, customer_id, kind, source_type, source_id, event) VALUES ($1,$2,'entitlement','subscription',$3,'grant')`,
			merchantID, customer, subGrant.String())
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=ANY($1)`, []uuid.UUID{subKeep, subGone, subGrant})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=ANY($1)`, []uuid.UUID{priceKeep, priceGone, priceGrant})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=ANY($1)`, []uuid.UUID{prodKeep, prodGone, prodGrant})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.provider_accounts WHERE id=$1`, paID)
			return nil
		})
	})

	// Snapshot keeps only subKeep; subGone + subGrant are absent (excess).
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		FetchedAt:    time.Now().UTC(),
		Capabilities: Capabilities{Subscriptions: true},
		Coverage:     SnapshotCoverage{SubscriptionsExhaustive: true},
		Subscriptions: []RemoteSubscription{
			{RailSubscriptionID: psubKeep, Status: SubscriptionStatusActive},
		},
	}
	fetcher := &fakeFetcher{provider: ProviderNMI, snap: snap}
	binding := ProviderAccountBinding{ID: paID, ProviderType: "nmi", AccountID: "acct-" + suffix}

	subExists := func(ctx context.Context, id uuid.UUID) bool {
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.subscriptions WHERE id=$1`, id).Scan(&n))
		return n == 1
	}

	// Incomplete provider coverage: every account-bound local row in the domain
	// is skipped, and none are compared to a partial snapshot.
	incompleteSnap := *snap
	incompleteSnap.Coverage = SnapshotCoverage{}
	incompleteFetcher := &fakeFetcher{provider: ProviderNMI, snap: &incompleteSnap}
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := PruneProviderAccountExcess(ctx, appDB, incompleteFetcher, ProviderNMI, binding, PruneParams{Apply: true})
		require.NoError(t, err)
		require.Equal(t, 0, res.Subscriptions)
		require.Equal(t, 3, res.SubscriptionsSkipped)
		require.Equal(t, "snapshot_not_complete", res.SubscriptionSkipReason)
		require.True(t, subExists(ctx, subKeep))
		require.True(t, subExists(ctx, subGone))
		require.True(t, subExists(ctx, subGrant))
		return nil
	}))

	// Dry-run: discovers excess, writes nothing.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := PruneProviderAccountExcess(ctx, appDB, fetcher, ProviderNMI, binding, PruneParams{Apply: false})
		require.NoError(t, err)
		require.Equal(t, 1, res.Subscriptions, "subGone would be deleted")
		require.Equal(t, 1, res.SubscriptionsSkipped, "subGrant is entangled -> skipped")
		require.True(t, subExists(ctx, subGone), "dry-run deletes nothing")
		require.True(t, subExists(ctx, subGrant))
		return nil
	}))

	// Apply: deletes the bare excess sub, skips the grant-entangled one.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := PruneProviderAccountExcess(ctx, appDB, fetcher, ProviderNMI, binding, PruneParams{Apply: true})
		require.NoError(t, err)
		require.Equal(t, 1, res.Subscriptions)
		require.Equal(t, 1, res.SubscriptionsSkipped)
		require.False(t, subExists(ctx, subGone), "bare excess sub deleted")
		require.True(t, subExists(ctx, subKeep), "present sub untouched")
		require.True(t, subExists(ctx, subGrant), "grant-entangled excess sub preserved for convergence retraction")
		return nil
	}))

	// Idempotent: subGone already gone; only the skipped grant sub remains excess.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := PruneProviderAccountExcess(ctx, appDB, fetcher, ProviderNMI, binding, PruneParams{Apply: true})
		require.NoError(t, err)
		require.Equal(t, 0, res.Subscriptions, "nothing left to delete")
		require.Equal(t, 1, res.SubscriptionsSkipped)
		return nil
	}))
}

func TestPruneProviderAccountExcess_PaymentsRequireCompleteWindow(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	suffix := uuid.NewString()[:8]
	paID := uuid.New()
	productID := uuid.New()
	priceID := uuid.New()
	payKeep := uuid.New()
	payGone := uuid.New()
	var customer uuid.UUID
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.provider_accounts (id, merchant_id, provider_type, account_id, role, status) VALUES ($1,$2,'nmi',$3,'secondary','enabled')`,
			paID, merchantID, "acct-pay-"+suffix)
		exec(`INSERT INTO openrails.products (id, slug, display_name, entitlements_spec, merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`,
			productID, "prune-pay-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, billing_cycle_days, merchant_id) VALUES ($1,$2,999,'usd',30,$3)`,
			priceID, productID, merchantID)
		exec(`INSERT INTO openrails.payments (id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, customer_id, merchant_id, provider_account_id)
		      VALUES ($1,$2,'nmi','txn-keep-' || $3,999,999,'usd','completed',$4,$5,$6,$7)`,
			payKeep, priceID, suffix, since.Add(24*time.Hour), customer, merchantID, paID)
		exec(`INSERT INTO openrails.payments (id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, customer_id, merchant_id, provider_account_id)
		      VALUES ($1,$2,'nmi','txn-gone-' || $3,999,999,'usd','completed',$4,$5,$6,$7)`,
			payGone, priceID, suffix, since.Add(48*time.Hour), customer, merchantID, paID)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE id=ANY($1)`, []uuid.UUID{payKeep, payGone})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.provider_accounts WHERE id=$1`, paID)
			return nil
		})
	})

	paymentExists := func(ctx context.Context, id uuid.UUID) bool {
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE id=$1`, id).Scan(&n))
		return n == 1
	}
	binding := ProviderAccountBinding{ID: paID, ProviderType: "nmi", AccountID: "acct-pay-" + suffix}
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		FetchedAt:    time.Now().UTC(),
		Capabilities: Capabilities{Transactions: true},
		Transactions: []RemoteTransaction{{TransactionID: "txn-keep-" + suffix, Type: TransactionTypeSale, Success: true, OccurredAt: since.Add(24 * time.Hour)}},
	}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := PruneProviderAccountExcess(ctx, appDB, &fakeFetcher{provider: ProviderNMI, snap: snap}, ProviderNMI, binding, PruneParams{Since: since, Until: until, Apply: true})
		require.NoError(t, err)
		require.Equal(t, 0, res.Payments)
		require.Equal(t, 2, res.PaymentsSkipped)
		require.Equal(t, "snapshot_not_complete", res.PaymentSkipReason)
		require.True(t, paymentExists(ctx, payKeep))
		require.True(t, paymentExists(ctx, payGone))
		return nil
	}))

	completeSnap := *snap
	completeSnap.Coverage = SnapshotCoverage{
		TransactionsExhaustive:        true,
		TransactionsPaginatedComplete: true,
		TransactionWindowSince:        timePtrIfSet(since),
		TransactionWindowUntil:        timePtrIfSet(until),
	}
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := PruneProviderAccountExcess(ctx, appDB, &fakeFetcher{provider: ProviderNMI, snap: &completeSnap}, ProviderNMI, binding, PruneParams{Since: since, Until: until, Apply: true})
		require.NoError(t, err)
		require.Equal(t, 1, res.Payments)
		require.Equal(t, 0, res.PaymentsSkipped)
		require.True(t, paymentExists(ctx, payKeep))
		require.False(t, paymentExists(ctx, payGone))
		return nil
	}))
}
