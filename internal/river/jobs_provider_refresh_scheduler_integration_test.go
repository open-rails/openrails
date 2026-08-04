//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #719 mid-upgrade dedupe proof: two scheduler passes back-to-back (a leftover
// pre-upgrade umbrella row worked next to the fresh periodic tick) must yield
// exactly ONE in-flight job per merchant — river's unique key, not timing.
// Also exercises the REAL RLS-scoped accounts predicate: no boot plane, so the
// merchant without a declared rail account never enqueues.
func TestProviderRefreshScheduler_UniquePerMerchantAcrossTicks(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	svc := pullTestMerchantsService(t, dbi)
	sfx := uuid.NewString()[:8]

	midA := seedPullMerchant(t, dbi, "sched-a-"+sfx)
	midB := seedPullMerchant(t, dbi, "sched-b-"+sfx)
	seedProviderAccount(t, svc, midA, "nmi", "9955"+sfx, map[string]string{"security_key": "sec-" + sfx})

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{SkipUnknownJobCheck: true})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM river_job WHERE kind = $1 AND args->>'merchant_id' = ANY($2::text[])`,
			KindProviderRefreshMerchant, []string{midA.UUID().String(), midB.UUID().String()})
	})

	sched := &ProviderRefreshSchedulerWorker{
		DB:       dbi,
		Inserter: client,
		// Jitter keeps jobs `scheduled` while the overlapping tick inserts.
		Stagger: 10 * time.Minute,
		ListMerchants: func(context.Context) ([]uuid.UUID, error) {
			return []uuid.UUID{midA.UUID(), midB.UUID()}, nil
		},
	}
	require.NoError(t, sched.Work(context.Background(), &river.Job[ProviderRefreshArgs]{}))
	require.NoError(t, sched.Work(context.Background(), &river.Job[ProviderRefreshArgs]{}))

	countFor := func(mid merchant.ID) int {
		var n int
		require.NoError(t, pool.QueryRow(context.Background(),
			`SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'merchant_id' = $2`,
			KindProviderRefreshMerchant, mid.UUID().String()).Scan(&n))
		return n
	}
	require.Equal(t, 1, countFor(midA), "two ticks, one in-flight job (river unique key)")
	require.Equal(t, 0, countFor(midB), "zero-accounts merchant never enqueues (no boot plane)")
}
