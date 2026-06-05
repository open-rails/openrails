//go:build integration

package service_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// TestGetUsage_Breakdown proves the facade method backing GET /v1/self/usage
// returns the per-event_type rollup (summed amount, event count, summed
// dimensions) for a seeded owner over a window (issue #289). It mirrors the
// credits-package usage rollup test but exercises the public service facade the
// HTTP handler calls.
func TestGetUsage_Breakdown(t *testing.T) {
	svc, cs, owner, ct, ctx := authzEnv(t)

	// Separate connection used only to clean up the usage_events rows this test
	// writes, scoped by owner.
	cleanupDB := bun.NewDB(
		sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.SharedPostgresDSN(t)))),
		pgdialect.New(),
	)
	models.RegisterModels(cleanupDB)
	t.Cleanup(func() {
		_, _ = cleanupDB.NewDelete().Model((*models.UsageEvent)(nil)).Where("owner_id = ?", owner.UUID()).Exec(ctx)
		_ = cleanupDB.Close()
	})

	_, err := cs.Deposit(ctx, credits.CreditDepositParams{
		OwnerID: &owner, UserID: owner.UUID().String(), CreditType: ct, Amount: 100_000, Source: "seed",
	})
	require.NoError(t, err)

	_, err = cs.RecordUsage(ctx, credits.RecordUsageParams{
		Owner: &owner, UserID: "user:a", CreditType: ct, EventType: "gpt-4o",
		Dimensions: map[string]int64{"input_tokens": 100, "output_tokens": 50},
		Amount:     5_000, Source: "req", SourceID: "u1",
	})
	require.NoError(t, err)
	_, err = cs.RecordUsage(ctx, credits.RecordUsageParams{
		Owner: &owner, UserID: "user:a", CreditType: ct, EventType: "gpt-4o",
		Dimensions: map[string]int64{"input_tokens": 60, "output_tokens": 30},
		Amount:     3_000, Source: "req", SourceID: "u2",
	})
	require.NoError(t, err)
	_, err = cs.RecordUsage(ctx, credits.RecordUsageParams{
		Owner: &owner, UserID: "user:a", CreditType: ct, EventType: "embeddings",
		Dimensions: map[string]int64{"input_tokens": 200},
		Amount:     1_000, Source: "req", SourceID: "u3",
	})
	require.NoError(t, err)

	rows, err := svc.GetUsage(ctx, owner, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byType := map[string]int{}
	for i, r := range rows {
		byType[r.EventType] = i
	}

	g := rows[byType["gpt-4o"]]
	require.Equal(t, int64(8_000), g.TotalAmount)
	require.Equal(t, int64(2), g.EventCount)
	require.Equal(t, int64(160), g.Dimensions["input_tokens"])
	require.Equal(t, int64(80), g.Dimensions["output_tokens"])

	e := rows[byType["embeddings"]]
	require.Equal(t, int64(1_000), e.TotalAmount)
	require.Equal(t, int64(1), e.EventCount)
	require.Equal(t, int64(200), e.Dimensions["input_tokens"])

	// A window before any usage returns no rows.
	empty, err := svc.GetUsage(ctx, owner, time.Now().Add(-72*time.Hour), time.Now().Add(-48*time.Hour))
	require.NoError(t, err)
	require.Empty(t, empty)
}
