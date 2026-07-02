//go:build integration

package converge

import (
	"context"
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
		_, err := q.UpsertRailMerchantAccount(ctx, gen.UpsertRailMerchantAccountParams{
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
			reconcile.PullProofs{reconcile.ProviderNMI: {}}, time.Now().UTC())
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
			}}}, time.Now().UTC())
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
		_, err = q.UpsertRailMerchantAccount(ctx, gen.UpsertRailMerchantAccountParams{
			MerchantID: gateMerchant.UUID(), Rail: "nmi", AccountID: "gate-nmi-b-" + suffix,
		})
		require.NoError(t, err)
		flipped, err = reconcile.MarkReconciledSourceDomains(ctx, q, gateMerchant.UUID(),
			reconcile.PullProofs{reconcile.ProviderNMI: {Coverage: reconcile.SnapshotCoverage{
				SubscriptionsExhaustive: true, TransactionsExhaustive: true, TransactionsPaginatedComplete: true,
			}}}, time.Now().UTC())
		require.NoError(t, err)
		require.Empty(t, flipped, "multi-account rail is unprovable by one pull")
		require.True(t, isReconciled(ctx, q, "subscriptions"), "ratchet: proven domains never unset")
		require.False(t, isReconciled(ctx, q, "payments"))
		return nil
	}))
}
