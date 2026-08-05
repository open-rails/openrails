//go:build integration

package converge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #665 Part B: the §3.2 confirmed-absence gate flips AUTOMATICALLY from what a
// pull actually proved — exhaustive coverage of every declared rail account —
// and a previously-held EXCESS repair proceeds once its domain flips. A
// non-exhaustive pull proves nothing; multi-account rails prove nothing; the
// flag is a ratchet.
func TestConverge_ConfirmedAbsenceGateFlipsOnExhaustivePull(t *testing.T) {
	appDB := startReconcilePostgres(t)
	suffix := uuid.NewString()[:8]
	// Dedicated merchant: the gate inspects the merchant's WHOLE declared
	// rail-account catalog, which other suites pollute on the shared merchant.
	gateMerchant := merchant.ID(uuid.New())
	baseCtx := merchant.WithID(context.Background(), gateMerchant)
	_, err := appDB.Pool().Exec(context.Background(),
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		gateMerchant.UUID(), "gate-"+suffix)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, table := range []string{"reconciliation_state", "rail_merchant_accounts"} {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.`+table+` WHERE merchant_id=$1`, gateMerchant.UUID())
			}
			return nil
		})
		_, _ = appDB.Pool().Exec(context.Background(), `DELETE FROM openrails.merchants WHERE id=$1`, gateMerchant.UUID())
	})

	e := NewConvergeEngine(appDB)
	repaired := 0
	heldFinding := func() ConvergeFinding {
		return ConvergeFinding{
			Type: "derive.grant.excess", Shape: ShapeExcess, Class: ClassAuto,
			Severity: "high", SubjectKey: "gate-test:" + suffix, Provider: "self",
			SourceDomain: DomainSubscriptions,
			Repair:       func(ctx context.Context) error { repaired++; return nil },
		}
	}
	isReconciled := func(ctx context.Context, q *gen.Queries, domain string) bool {
		v, err := q.IsSourceDomainReconciled(ctx, gen.IsSourceDomainReconciledParams{
			MerchantID: gateMerchant.UUID(), SourceDomain: domain,
		})
		require.NoError(t, err)
		return v != nil && *v
	}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		q := appDB.Gen(ctx)
		scope := Scope{Merchant: gateMerchant}
		_, err := q.UpsertPSP(ctx, gen.UpsertPSPParams{
			MerchantID: gateMerchant.UUID(), Rail: "nmi", AccountID: "gate-nmi-" + suffix,
		})
		require.NoError(t, err)

		// Gate closed: the EXCESS repair is HELD (reconcile_required, not run).
		status, err := e.remediate(ctx, scope, heldFinding())
		require.NoError(t, err)
		require.Equal(t, "reconcile_required", status, "EXCESS repair held before the domain is proven")
		require.Zero(t, repaired)

		// A NON-exhaustive pull proves nothing.
		flipped, err := reconcile.MarkReconciledSourceDomains(ctx, q, gateMerchant.UUID(),
			reconcile.PullProofs{reconcile.ProviderNMI: {}})
		require.NoError(t, err)
		require.Empty(t, flipped, "non-exhaustive coverage must not flip any domain")
		require.False(t, isReconciled(ctx, q, "subscriptions"))

		// An exhaustive roster pull (bounded transaction window) proves
		// subscriptions ONLY — payments needs full-history coverage.
		since := time.Now().UTC().Add(-24 * time.Hour)
		flipped, err = reconcile.MarkReconciledSourceDomains(ctx, q, gateMerchant.UUID(),
			reconcile.PullProofs{reconcile.ProviderNMI: {Coverage: reconcile.SnapshotCoverage{
				SubscriptionsExhaustive: true, TransactionsExhaustive: true,
				TransactionsPaginatedComplete: true, TransactionWindowSince: &since,
			}}})
		require.NoError(t, err)
		require.Equal(t, []string{"subscriptions"}, flipped)
		require.True(t, isReconciled(ctx, q, "subscriptions"))
		require.False(t, isReconciled(ctx, q, "payments"))

		// Gate open: the previously-held repair proceeds on the next converge.
		status, err = e.remediate(ctx, scope, heldFinding())
		require.NoError(t, err)
		require.Equal(t, "auto_fixed", status)
		require.Equal(t, 1, repaired)

		// A second account on the rail: a single-credential pull can no longer
		// prove anything — payments stays unproven even with full coverage, and
		// the already-proven subscriptions domain stays true (ratchet).
		_, err = q.UpsertPSP(ctx, gen.UpsertPSPParams{
			MerchantID: gateMerchant.UUID(), Rail: "nmi", AccountID: "gate-nmi-b-" + suffix,
		})
		require.NoError(t, err)
		flipped, err = reconcile.MarkReconciledSourceDomains(ctx, q, gateMerchant.UUID(),
			reconcile.PullProofs{reconcile.ProviderNMI: {Coverage: reconcile.SnapshotCoverage{
				SubscriptionsExhaustive: true, TransactionsExhaustive: true, TransactionsPaginatedComplete: true,
			}}})
		require.NoError(t, err)
		require.Empty(t, flipped, "multi-account rail is unprovable by one pull")
		require.True(t, isReconciled(ctx, q, "subscriptions"), "ratchet: proven domains never unset")
		require.False(t, isReconciled(ctx, q, "payments"))
		return nil
	}))
}

// or#842 residual: the same §3.2 gate, opened by an EMPTY Stripe roster.
//
// The gate is a RATCHET — once `subscriptions` flips true nothing ever unsets
// it, and every destructive EXCESS repair for that merchant proceeds from then
// on without re-consulting any roster. Stripe's fetcher stamped
// SubscriptionsExhaustive before its list call had even run, so ONE successful
// but empty pull (a key rotated onto a sibling account, a restricted key, an
// incident answering `{"data":[],"has_more":false}`) permanently certified "this
// merchant has no subscribers" and unlocked the retractions.
func TestConverge_EmptyStripeRosterNeverOpensTheAbsenceGate(t *testing.T) {
	appDB := startReconcilePostgres(t)
	suffix := uuid.NewString()[:8]
	gateMerchant := merchant.ID(uuid.New())
	baseCtx := merchant.WithID(context.Background(), gateMerchant)
	_, err := appDB.Pool().Exec(context.Background(),
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		gateMerchant.UUID(), "gate-stripe-"+suffix)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, table := range []string{"reconciliation_state", "psps"} {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.`+table+` WHERE merchant_id=$1`, gateMerchant.UUID())
			}
			return nil
		})
		_, _ = appDB.Pool().Exec(context.Background(), `DELETE FROM openrails.merchants WHERE id=$1`, gateMerchant.UUID())
	})

	// A live Stripe answering every list with an empty page.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[]}`))
	}))
	t.Cleanup(server.Close)
	fetcher := &reconcile.StripeFetcher{SecretKey: "sk_test_x", BaseURL: server.URL, HTTPClient: server.Client()}
	snap, err := fetcher.Fetch(context.Background(), reconcile.FetchParams{Since: time.Now().UTC().Add(-24 * time.Hour), Until: time.Now().UTC()})
	require.NoError(t, err)
	require.Empty(t, snap.Subscriptions)

	e := NewConvergeEngine(appDB)
	retracted := 0
	retraction := func() ConvergeFinding {
		return ConvergeFinding{
			Type: "derive.grant.excess", Shape: ShapeExcess, Class: ClassAuto,
			Severity: "high", SubjectKey: "gate-stripe:" + suffix, Provider: "stripe",
			SourceDomain: DomainSubscriptions,
			Repair:       func(ctx context.Context) error { retracted++; return nil },
		}
	}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		q := appDB.Gen(ctx)
		psp, err := q.UpsertPSP(ctx, gen.UpsertPSPParams{
			MerchantID: gateMerchant.UUID(), Rail: "stripe", AccountID: "acct_" + suffix,
		})
		require.NoError(t, err)

		flipped, err := reconcile.MarkReconciledSourceDomains(ctx, q, gateMerchant.UUID(),
			reconcile.PullProofs{reconcile.ProviderStripe: {Coverage: snap.Coverage, PspID: psp.ID.String()}})
		require.NoError(t, err)
		require.Empty(t, flipped,
			"an empty Stripe roster proved absence — the confirmed-absence gate is a ratchet, so this opens every retraction for this merchant permanently")

		reconciled, err := q.IsSourceDomainReconciled(ctx, gen.IsSourceDomainReconciledParams{
			MerchantID: gateMerchant.UUID(), SourceDomain: "subscriptions",
		})
		require.NoError(t, err)
		require.True(t, reconciled == nil || !*reconciled)

		status, err := e.remediate(ctx, Scope{Merchant: gateMerchant}, retraction())
		require.NoError(t, err)
		require.Equal(t, "reconcile_required", status)
		require.Zero(t, retracted, "%d retractions ran off an empty roster", retracted)
		return nil
	}))
}
