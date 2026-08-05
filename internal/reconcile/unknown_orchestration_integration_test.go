//go:build integration

package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// (fakeFetcher is defined in engine_test.go.)

// fakeProber is a canned per-subscription snapshot source (#665).
type fakeProber struct {
	snaps map[string]*RemoteSnapshot // by rail_subscription_id
	calls []ProbeSubject
}

func (p *fakeProber) ProbeSubscription(ctx context.Context, subj ProbeSubject) (*RemoteSnapshot, error) {
	p.calls = append(p.calls, subj)
	if snap := p.snaps[subj.RailSubscriptionID]; snap != nil {
		return snap, nil
	}
	return nil, errors.New("no canned snapshot")
}

// #633/#634/#665 end-to-end with a FAKE windowed fetcher (fixture provider
// truth) plus a per-sub probe fallback: the `unknown` cohort is resolved against
// one bulk snapshot per rail — renewed / adopted / past_due / cancelled —
// missing charges are backfilled (declines as failed), a NULL-period legacy row
// is resolved by the targeted probe, and a rail whose fetch fails leaves its
// subs `unknown` (RailErrors), proving the backoff hand-off.
func TestReconcileUnknownCohort_FixtureSnapshot(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	lc := subscriptions.NewSubscriptionLifecycleService(appDB, nil, nil, nil, nil, nil, clockwork.NewRealClock())
	now := time.Now().UTC().Truncate(time.Second) // second precision: adopted ends round-trip through timestamptz
	periodEnd := now.Add(-5 * 24 * time.Hour)     // lapsed 5d ago (within dunning window)
	start := now.Add(-35 * 24 * time.Hour)
	nextEnd := now.Add(25 * 24 * time.Hour)
	sfx := uuid.NewString()[:8]

	// rail_subscription_ids drive the match.
	rsRenew, rsCancel, rsDecline, rsNMI, rsSolana := "rs-renew-"+sfx, "rs-cancel-"+sfx, "rs-decline-"+sfx, "rs-nmi-"+sfx, "rs-sol-"+sfx
	rsProbe := "rs-probe-" + sfx
	subRenew, subCancel, subDecline, subNMI, subSolana := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	subProbe := uuid.New()
	nmiVault := "vault-" + sfx
	prices := map[uuid.UUID]uuid.UUID{}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		pspByRail := map[string]uuid.UUID{
			"ccbill": dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantID, "ccbill"),
			"nmi":    dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantID, "nmi"),
			"solana": dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantID, "solana"),
		}
		// Distinct customer + product + price per sub (uq_subscriptions_customer_product_lifecycle).
		mk := func(id uuid.UUID, rail, railSub string) uuid.UUID {
			cust := dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
			prod, price := uuid.New(), uuid.New()
			prices[id] = price
			exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`, prod, "ur-"+railSub, merchantID)
			exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,merchant_id) VALUES ($1,$2,5000000,'USD',$3)`, price, prod, merchantID)
			exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at,psp_id)
			      VALUES ($1,$2,$3,$4,$5,'unknown',$6,$7,$8,$8,$9,$10)`, id, merchantID, cust, prod, price, rail, railSub, start, periodEnd, pspByRail[rail])
			return cust
		}
		mk(subRenew, "ccbill", rsRenew)
		custCancel := mk(subCancel, "ccbill", rsCancel)
		mk(subDecline, "ccbill", rsDecline)
		mk(subNMI, "nmi", rsNMI)
		mk(subSolana, "solana", rsSolana)
		mk(subProbe, "nmi", rsProbe)
		// Legacy import shape: NO local period evidence — only the per-sub probe
		// can resolve it (#665).
		exec(`UPDATE openrails.subscriptions SET current_period_starts_at=NULL, current_period_ends_at=NULL WHERE id=$1`, subProbe)
		// A lingering entitlement window on the to-be-cancelled sub so the
		// revocation is observable (ported #367 remote-absent scenario).
		exec(`INSERT INTO openrails.entitlements (id, entitlement, start_at, end_at, source_id, source_type, customer_id, merchant_id)
		      VALUES ($1,'premium', now() - interval '40 days', now() + interval '10 days', $2, 'subscription', $3, $4)`,
			uuid.New(), subCancel, custCancel, merchantID)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.rail_customer_accounts WHERE account_id=$1`, nmiVault)
			for id, price := range prices {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE source_id=$1`, id)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE subscription_id=$1`, id)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, id)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			}
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE key LIKE 'ur-rs-%'||$1`, sfx)
			return nil
		})
	})

	ccbillSnap := &RemoteSnapshot{
		Provider: ProviderCCBill,
		Subscriptions: []RemoteSubscription{
			{RailSubscriptionID: rsRenew, Status: SubscriptionStatusActive, NextBillingAt: &nextEnd},
			{RailSubscriptionID: rsCancel, Status: SubscriptionStatusCancelled},
		},
		Transactions: []RemoteTransaction{
			{TransactionID: "tx-renew-" + sfx, SubscriptionID: rsRenew, Type: TransactionTypeSale, Success: true, AmountCents: 5000, Currency: "USD", OccurredAt: periodEnd.Add(time.Hour)},
			{TransactionID: "tx-decline-" + sfx, SubscriptionID: rsDecline, Type: TransactionTypeDecline, Success: false, OccurredAt: periodEnd.Add(time.Hour)},
		},
	}
	// #635/#682: an NMI vault sub — the remote sub carries a customer_vault_id,
	// but vault ids are per-card instrument containers, not persons, so NO
	// rail_customer_accounts row is materialized for it (Stripe cus_* only).
	// Roster-active with a future boundary and NO charge → ADOPTED, not renewed
	// (#367 doctrine: period adoption alone never grants access).
	nmiSnap := &RemoteSnapshot{
		Provider: ProviderNMI,
		Subscriptions: []RemoteSubscription{
			{RailSubscriptionID: rsNMI, Status: SubscriptionStatusActive, CustomerID: nmiVault, NextBillingAt: &nextEnd},
		},
	}
	fetchers := map[Provider]RailFetcher{
		ProviderCCBill: &fakeFetcher{provider: ProviderCCBill, snap: ccbillSnap},
		ProviderNMI:    &fakeFetcher{provider: ProviderNMI, snap: nmiSnap},
		ProviderSolana: &fakeFetcher{provider: ProviderSolana, err: errors.New("solana unreachable")}, // backoff path
	}
	// #665 probe fallback: the NULL-period legacy row is invisible to the bulk
	// window; the per-sub probe answers with an alive remote + future billing.
	probeEnd := now.Add(72 * time.Hour)
	nmiProber := &fakeProber{snaps: map[string]*RemoteSnapshot{
		rsProbe: {
			Provider:      ProviderNMI,
			Coverage:      SnapshotCoverage{SubscriptionsExhaustive: true},
			Subscriptions: []RemoteSubscription{{RailSubscriptionID: rsProbe, Status: SubscriptionStatusActive, NextBillingAt: &probeEnd}},
		},
	}}
	probers := map[Provider]SubscriptionProber{ProviderNMI: nmiProber}

	var res UnknownReconcileResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var err error
		res, err = ReconcileUnknownCohort(ctx, appDB, lc, fetchers, probers, dbtest.TestMerchantID, now, UnknownReconcileOptions{})
		return err
	}))

	// Backoff: the Solana fetch failed → its sub stays unknown + a RailError recorded.
	require.Contains(t, res.RailErrors, ProviderSolana)
	require.GreaterOrEqual(t, res.StillUnknown, 1)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		status := func(id uuid.UUID) string {
			var s string
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.subscriptions WHERE id=$1`, id).Scan(&s))
			return s
		}
		payCount := func(id uuid.UUID, st string) int {
			var n int
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE subscription_id=$1 AND status=$2`, id, st).Scan(&n))
			return n
		}
		railCustomerCount := func(id uuid.UUID, ridVal string) int {
			var n int
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
				`SELECT count(*) FROM openrails.rail_customer_accounts rc JOIN openrails.subscriptions s ON s.customer_id = rc.customer_id
				 WHERE s.id=$1 AND rc.rail='nmi' AND rc.account_id=$2`, id, ridVal).Scan(&n))
			return n
		}
		require.Equal(t, "active", status(subRenew), "successful renewal → active")
		require.Equal(t, 1, payCount(subRenew, "completed"), "renewal charge backfilled as completed")
		require.Equal(t, "cancelled", status(subCancel), "roster cancelled → cancelled")
		var liveEnts int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE source_id=$1 AND revoked_at IS NULL AND deleted_at IS NULL AND (end_at IS NULL OR end_at > now())`, subCancel).Scan(&liveEnts))
		require.Zero(t, liveEnts, "entitlements must be revoked with the cancellation (ported #367)")
		require.Equal(t, "past_due", status(subDecline), "declined within window → past_due")
		require.Equal(t, 1, payCount(subDecline, "failed"), "declined charge backfilled as failed")
		require.Equal(t, "active", status(subNMI), "nmi roster-alive sub adopted → active")
		var adoptedEnd *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT current_period_ends_at FROM openrails.subscriptions WHERE id=$1`, subNMI).Scan(&adoptedEnd))
		require.NotNil(t, adoptedEnd)
		require.True(t, adoptedEnd.Equal(nextEnd), "remote next billing adopted as the local period end")
		require.Zero(t, payCount(subNMI, "completed"), "adoption never fabricates a charge")
		require.Equal(t, 0, railCustomerCount(subNMI, nmiVault), "#682: nmi vault is per-card, not a person — no rail_customer_accounts row")
		require.Equal(t, "unknown", status(subSolana), "unreachable rail → stays unknown")
		// #665: the NULL-period legacy row was resolved by the targeted probe.
		require.Equal(t, "active", status(subProbe), "NULL-period row adopted via per-sub probe")
		var probedEnd *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT current_period_ends_at FROM openrails.subscriptions WHERE id=$1`, subProbe).Scan(&probedEnd))
		require.NotNil(t, probedEnd)
		require.True(t, probedEnd.Equal(probeEnd), "probe-sourced next billing adopted")
		return nil
	}))

	require.Equal(t, 1, res.Renewed)
	require.Equal(t, 2, res.Adopted) // nmi roster-alive + NULL-period probe
	require.Equal(t, 1, res.PastDue)
	require.Equal(t, 1, res.Cancelled)
	require.Equal(t, 2, res.Backfilled)
	// #682: NMI vault ids are per-card instrument containers, not persons — no
	// rail_customer_accounts rows are materialized for NMI anymore (Stripe cus_* only,
	// and this fixture has no Stripe subject).
	require.Equal(t, 0, res.RailCustomers)
	// The probe fired ONLY for the row the bulk snapshot could not decide.
	require.Len(t, nmiProber.calls, 1)
	require.Equal(t, subProbe, nmiProber.calls[0].LocalID)
	require.Nil(t, nmiProber.calls[0].PeriodEnd)

	// Exactly-once: a second pass finds no unknown rows for the resolved subs —
	// no duplicate payments, statuses stable (ported #367 charged-repair rerun).
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res2, err := ReconcileUnknownCohort(ctx, appDB, lc, fetchers, probers, dbtest.TestMerchantID, now, UnknownReconcileOptions{})
		require.NoError(t, err)
		require.Zero(t, res2.Renewed+res2.Adopted+res2.PastDue+res2.Cancelled)
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE subscription_id=$1`, subRenew).Scan(&n))
		require.Equal(t, 1, n, "a second pass must not duplicate the backfilled payment")
		return nil
	}))
}
