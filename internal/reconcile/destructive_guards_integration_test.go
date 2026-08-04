//go:build integration

package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// The go-live blockers, end to end on the real path — real Postgres, the real
// lifecycle service, real entitlement rows. Every case here cancelled real
// customers and revoked their access before the fix.

type guardCohort struct {
	subs     []uuid.UUID
	railSubs []string
	suffix   string
}

// seedGuardCohort creates n NMI subscriptions in `status`, each with a live
// `premium` entitlement window, and registers their cleanup.
func seedGuardCohort(t *testing.T, appDB *db.DB, baseCtx context.Context, n int, status string, periodEnd time.Time) guardCohort {
	t.Helper()
	merchantID := dbtest.TestMerchantID.UUID()
	c := guardCohort{suffix: uuid.NewString()[:8]}
	start := periodEnd.Add(-30 * 24 * time.Hour)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		for i := 0; i < n; i++ {
			cust := dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
			prod, price, sub := uuid.New(), uuid.New(), uuid.New()
			rs := fmt.Sprintf("rs-guard-%s-%d", c.suffix, i)
			key := fmt.Sprintf("guard-%s-%d", c.suffix, i)
			exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id)
			      VALUES ($1,$2,$2,jsonb_build_object('premium',null),$3)`, prod, key, merchantID)
			exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id)
			      VALUES ($1,$2,5000000,'USD',720,true,$3)`, price, prod, merchantID)
			exec(`INSERT INTO openrails.subscriptions
			        (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,
			         started_at,current_period_starts_at,current_period_ends_at,entitlements_spec_snapshot)
			      VALUES ($1,$2,$3,$4,$5,$6,'nmi',$7,$8,$8,$9,jsonb_build_object('premium',null))`,
				sub, merchantID, cust, prod, price, status, rs, start, periodEnd)
			exec(`INSERT INTO openrails.entitlements (merchant_id,customer_id,entitlement,start_at,end_at,source_id,source_type)
			      VALUES ($1,$2,'premium',$3,$4,$5,'subscription')`,
				merchantID, cust, start, periodEnd.Add(720*time.Hour), sub)
			c.subs = append(c.subs, sub)
			c.railSubs = append(c.railSubs, rs)
		}
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, sub := range c.subs {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.rail_intents WHERE subscription_id=$1`, sub)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE source_id=$1`, sub)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE subscription_id=$1`, sub)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, sub)
			}
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE product_id IN (SELECT id FROM openrails.products WHERE key LIKE 'guard-'||$1||'-%')`, c.suffix)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE key LIKE 'guard-'||$1||'-%'`, c.suffix)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE finding_type=$1`, string(FindingCancellationCapped))
			return nil
		})
	})
	return c
}

// guardCounts reads how many of the cohort survived and how many entitlement
// windows are still live.
func guardCounts(t *testing.T, appDB *db.DB, baseCtx context.Context, c guardCohort) (cancelled, liveEntitlements int) {
	t.Helper()
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.subscriptions WHERE id = ANY($1) AND status = 'cancelled'`, c.subs).Scan(&cancelled))
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE source_id = ANY($1) AND revoked_at IS NULL`, c.subs).Scan(&liveEntitlements))
		return nil
	}))
	return cancelled, liveEntitlements
}

func guardFindings(t *testing.T, appDB *db.DB, baseCtx context.Context, findingType FindingType) []string {
	t.Helper()
	var out []string
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		rows, err := appDB.Qx(ctx).Query(ctx,
			`SELECT subject_key FROM openrails.reconciliation_findings
			  WHERE merchant_id=$1 AND finding_type=$2 AND status='requires_review'`,
			dbtest.TestMerchantID.UUID(), string(findingType))
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var k string
			require.NoError(t, rows.Scan(&k))
			out = append(out, k)
		}
		return rows.Err()
	}))
	return out
}

func guardLifecycle(appDB *db.DB) *subscriptions.SubscriptionLifecycleService {
	return subscriptions.NewSubscriptionLifecycleService(appDB, nil, nil, nil, nil, nil, clockwork.NewRealClock())
}

// #834: an empty roster used to cancel up to 500 customers per rail per
// merchant per pass. Those cancels carry RemoteGone=true, so they create NO
// provider intent and the #679 volume breaker never saw a single one of them.
// The trigger is mundane: a 200 with zero rows from GET /v5/subscriptions.
func TestUnknownCohort_EmptyRosterCancelsNothingAndRaisesAFinding(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)
	cohort := seedGuardCohort(t, appDB, baseCtx, 40, "unknown", now.Add(-60*24*time.Hour))

	// The shape a misdeclared psps.account_id, a credential rotated onto a
	// sibling sub-account, or an NMI incident produces: a successful,
	// pagination-complete, EMPTY roster.
	empty := &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Subscriptions: true}}
	fetchers := map[Provider]RailFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: empty}}

	var res UnknownReconcileResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var err error
		res, err = ReconcileUnknownCohort(ctx, appDB, guardLifecycle(appDB), fetchers, nil, dbtest.TestMerchantID, now, UnknownReconcileOptions{})
		return err
	}))

	require.Zero(t, res.Cancelled, "an empty roster must cancel nobody")
	cancelled, live := guardCounts(t, appDB, baseCtx, cohort)
	require.Zero(t, cancelled, "%d subscriptions were cancelled off an empty roster", cancelled)
	require.Equal(t, len(cohort.subs), live, "entitlements must survive: access is never lost to our own malfunction")

	subjects := guardFindings(t, appDB, baseCtx, FindingCancellationCapped)
	require.NotEmpty(t, subjects, "a withheld mass cancellation must reach the operator queue, not just the log")
	require.Contains(t, subjects, "nmi:unknown_cohort:roster_ratio")
}

// #837: the 10% ratio breaker leaves a 90-point hole. A roster truncated to
// 15% clears it cleanly and would cancel 85% of the book.
func TestUnknownCohort_TruncatedRosterTripsTheCancellationCap(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)
	cohort := seedGuardCohort(t, appDB, baseCtx, 40, "unknown", now.Add(-60*24*time.Hour))

	// 15% of the book comes back (6 of 40) — comfortably above the ratio
	// breaker's 10% floor, so only the cap can stop this.
	next := now.Add(20 * 24 * time.Hour)
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true},
		Coverage:     SnapshotCoverage{SubscriptionsExhaustive: true},
	}
	for i := 0; i < 6; i++ {
		snap.Subscriptions = append(snap.Subscriptions, RemoteSubscription{
			RailSubscriptionID: cohort.railSubs[i], Status: SubscriptionStatusActive, NextBillingAt: &next,
		})
	}
	fetchers := map[Provider]RailFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: snap}}

	var res UnknownReconcileResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var err error
		res, err = ReconcileUnknownCohort(ctx, appDB, guardLifecycle(appDB), fetchers, nil, dbtest.TestMerchantID, now, UnknownReconcileOptions{})
		return err
	}))

	require.Zero(t, res.Cancelled)
	require.Equal(t, 34, res.Held, "all 34 planned cancellations withheld — the cap is all-or-nothing")
	cancelled, live := guardCounts(t, appDB, baseCtx, cohort)
	require.Zero(t, cancelled)
	require.Equal(t, len(cohort.subs), live)
	require.Contains(t, guardFindings(t, appDB, baseCtx, FindingCancellationCapped), "nmi:unknown_cohort:cancellation_cap")
}

// #837: below ten live subscriptions the ratio breaker was DISABLED entirely,
// so a nine-subscriber merchant could lose all nine.
func TestUnknownCohort_NineSubscriberMerchantIsProtected(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)
	cohort := seedGuardCohort(t, appDB, baseCtx, 9, "unknown", now.Add(-60*24*time.Hour))

	empty := &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Subscriptions: true}}
	fetchers := map[Provider]RailFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: empty}}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := ReconcileUnknownCohort(ctx, appDB, guardLifecycle(appDB), fetchers, nil, dbtest.TestMerchantID, now, UnknownReconcileOptions{})
		require.NoError(t, err)
		require.Zero(t, res.Cancelled)
		return nil
	}))

	cancelled, live := guardCounts(t, appDB, baseCtx, cohort)
	require.Zero(t, cancelled, "small merchants need MORE protection, not an exemption")
	require.Equal(t, 9, live)
}

// #821 + #835: an imported legacy NMI book. Every row is on the roster with a
// next_billing_date wedged in the past, because NMI stops advancing that date
// while rebills fail and retries forever. There is no decline evidence
// anywhere. This is what the first enforcing pass saw on boot.
func TestUnknownCohort_StaleRosterDatesNeverCancel(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)

	for _, staleDays := range []int{15, 30, 90} {
		t.Run(fmt.Sprintf("%dd_stale", staleDays), func(t *testing.T) {
			periodEnd := now.AddDate(0, 0, -staleDays)
			// Three rows, deliberately WITHIN the per-pass cancellation cap, so
			// this test isolates the decider law (#821) rather than passing
			// because a volume guard happened to catch it.
			cohort := seedGuardCohort(t, appDB, baseCtx, 3, "unknown", periodEnd)

			// The roster LISTS every one of them — alive at the rail, wedged.
			snap := &RemoteSnapshot{
				Provider:     ProviderNMI,
				Capabilities: Capabilities{Subscriptions: true},
				Coverage:     SnapshotCoverage{SubscriptionsExhaustive: true},
			}
			wedged := periodEnd
			for _, rs := range cohort.railSubs {
				snap.Subscriptions = append(snap.Subscriptions, RemoteSubscription{
					RailSubscriptionID: rs,
					Status:             SubscriptionStatusPastDue, // inferred from the date, never declared by NMI
					NextBillingAt:      &wedged,
				})
			}
			fetchers := map[Provider]RailFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: snap}}

			require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
				res, err := ReconcileUnknownCohort(ctx, appDB, guardLifecycle(appDB), fetchers, nil, dbtest.TestMerchantID, now, UnknownReconcileOptions{})
				require.NoError(t, err)
				require.Zero(t, res.Cancelled, "a lapsed next_billing_date is a dunning state, not a death certificate")
				return nil
			}))

			cancelled, live := guardCounts(t, appDB, baseCtx, cohort)
			require.Zero(t, cancelled, "%d-day-stale roster rows were cancelled with no decline evidence", staleDays)
			require.Equal(t, len(cohort.subs), live)

			// No deferred NMI vault delete was queued either — that is the
			// irreversible half.
			var intents int
			require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
				return appDB.Qx(ctx).QueryRow(ctx,
					`SELECT count(*) FROM openrails.rail_intents WHERE subscription_id = ANY($1)`, cohort.subs).Scan(&intents)
			}))
			require.Zero(t, intents, "a stale date must never queue the irreversible NMI delete")
		})
	}
}

// #841: a merchant running two active NMI PSPs. The pull arms from ONE of them,
// so the other's entire book is absent from the roster — and used to be
// cancelled as "absent from an exhaustive roster".
func TestPull_SecondPSPBookSurvivesASinglePSPPull(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	now := time.Now().UTC().Truncate(time.Second)

	pulled := seedGuardCohort(t, appDB, baseCtx, 6, "active", now.Add(30*24*time.Hour))
	unpulled := seedGuardCohort(t, appDB, baseCtx, 6, "active", now.Add(30*24*time.Hour))

	// The armed PSP's roster: complete and exhaustive FOR ITS OWN ACCOUNT.
	next := now.Add(30 * 24 * time.Hour)
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true},
		Coverage:     SnapshotCoverage{SubscriptionsExhaustive: true},
	}
	for _, rs := range pulled.railSubs {
		snap.Subscriptions = append(snap.Subscriptions, RemoteSubscription{
			RailSubscriptionID: rs, Status: SubscriptionStatusActive, NextBillingAt: &next,
		})
	}

	eng := &Engine{
		Fetchers:  map[Provider]RailFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: snap}},
		Store:     &PGStore{DB: appDB},
		Local:     &PGLocalStateLoader{DB: appDB},
		Writer:    &PGLocalWriter{DB: appDB},
		Decisions: NewDecisionApplier(appDB, nil),
		// Wired so a regression fails on THIS test's assertion (the sibling PSP's
		// book was cancelled) rather than on or#859's no-bypass gate, which would
		// otherwise refuse the pass first and hide what broke.
		Runs: &PGDestructiveRunRecorder{DB: appDB},
		Now:  func() time.Time { return now },
	}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := eng.Run(ctx, RunParams{
			Mode:      ModeEnforce,
			Mutations: &LocalMutationPolicy{Insert: false, Overwrite: true},
			Providers: []Provider{ProviderNMI},
			// Two PSPs declared active on nmi; this pass read one.
			PSPCoverage: map[Provider]PSPCoverage{ProviderNMI: {Declared: 2, Pulled: 1}},
		})
		return err
	}))

	cancelled, live := guardCounts(t, appDB, baseCtx, unpulled)
	require.Zero(t, cancelled, "the sibling PSP's whole book was cancelled as 'absent from an exhaustive roster'")
	require.Equal(t, len(unpulled.subs), live)
}
