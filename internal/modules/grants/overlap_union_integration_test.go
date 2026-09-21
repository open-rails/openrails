//go:build integration

package grants_test

import (
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/stretchr/testify/require"
	"math/rand"
	"testing"
	"time"
)

func TestEntitlementSourceUnionAndRevocation(t *testing.T) {
	l, pool, ctx, customer, product, mid := testGrants(t)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	type interval struct {
		lo, hi            int
		infinite, revoked bool
		grant             gen.OpenrailsGrant
	}
	// partial, contained, adjacent, infinite and duplicate-coverage sources.
	intervals := []interval{{lo: 0, hi: 10}, {lo: 5, hi: 15}, {lo: 6, hi: 8}, {lo: 15, hi: 20}, {lo: 18, infinite: true}, {lo: 19, infinite: true}}
	random := rand.New(rand.NewSource(994))
	for range 32 {
		lo := random.Intn(45)
		intervals = append(intervals, interval{lo: lo, hi: lo + 1 + random.Intn(15)})
	}
	for i := range intervals {
		w := &intervals[i]
		var end *time.Time
		if !w.infinite {
			e := start.Add(time.Duration(w.hi) * time.Hour)
			end = &e
		}
		var err error
		w.grant, err = l.Grant(ctx, grants.GrantInput{Customer: customer, Product: &product, Kind: grants.Entitlement,
			Source: grants.Purchase, SourceID: "union-" + short(), Spec: &grants.Spec{Entitlements: []string{"union"}},
			StartsAt: start.Add(time.Duration(w.lo) * time.Hour), EndsAt: end})
		require.NoError(t, err)
		require.NoError(t, l.MaterializeGrant(ctx, w.grant))
		require.NoError(t, l.MaterializeGrant(ctx, w.grant))
	}
	assertUnion := func() {
		t.Helper()
		for halfHour := -1; halfHour < 130; halfHour++ {
			at := float64(halfHour) / 2
			want := false
			for _, w := range intervals {
				if !w.revoked && at >= float64(w.lo) && (w.infinite || at < float64(w.hi)) {
					want = true
				}
			}
			got, err := dbtest.Queries(pool).EntitlementExistsActive(ctx, gen.EntitlementExistsActiveParams{MerchantID: mid, CustomerID: customer,
				Entitlement: "union", At: start.Add(time.Duration(halfHour) * 30 * time.Minute)})
			require.NoError(t, err)
			require.Equal(t, want, got, "coverage at %v hours", at)
		}
		var count int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND entitlement='union'`, mid, customer).Scan(&count))
		require.Equal(t, len(intervals), count, "replay must not duplicate projections")
		missing, err := l.MissingEffects(ctx, &customer)
		require.NoError(t, err)
		require.Empty(t, missing)
	}
	assertUnion()
	for _, i := range []int{0, 4, 5, 1, 2} {
		w := &intervals[i]
		require.NoError(t, l.RevokeBySourceAsOf(ctx, customer, grants.Entitlement, []grants.SourceType{grants.Purchase}, w.grant.SourceID, "refund", start))
		require.NoError(t, l.RevokeBySourceAsOf(ctx, customer, grants.Entitlement, []grants.SourceType{grants.Purchase}, w.grant.SourceID, "refund", start))
		require.NoError(t, l.MaterializeGrant(ctx, w.grant))
		require.NoError(t, l.MaterializeGrant(ctx, w.grant))
		w.revoked = true
		assertUnion()
	}
}
