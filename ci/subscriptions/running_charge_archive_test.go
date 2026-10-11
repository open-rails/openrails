//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchantarchive"
)

// A merchant archive carries running charges as they stand, a running one
// included, and a restore watches every running one again: the watch is
// derived, not archived.
func TestRunningChargesSurviveAMerchantArchive(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	payer := w.newCustomer()
	_, err := c.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{
		{CustomerID: payer.cid(), Currency: "USD", Amount: 10_000_000, Source: "support", SourceID: "seed"},
	})
	require.NoError(t, err)
	startedAt := w.clock.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	start := func(reference string) {
		t.Helper()
		_, err := c.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{
			Reference: reference, CustomerID: payer.cid(), Currency: "USD", RatePerHour: 3_600_000, StartedAt: &startedAt, Resource: "gpu:a100",
		})
		require.NoError(t, err)
	}
	start("archive-stopped")
	stoppedAt := startedAt.Add(30 * time.Minute)
	_, err = c.StopRunningCharge(t.Context(), "archive-stopped", billing.StopRunningChargeParams{StoppedAt: &stoppedAt})
	require.NoError(t, err)
	start("archive-running")

	merchantID := c.MerchantID()
	w.stop()
	source, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	var artifact bytes.Buffer
	err = merchantarchive.Export(t.Context(), source, merchantID, &artifact)
	require.NoError(t, err, "archive cause: %v", errors.Unwrap(err))

	schema := "archive_charges_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	require.NoError(t, engine.Migrate(t.Context(), w.pool, openrails.Config{Database: openrails.DatabaseConfig{Schema: schema, RiverSchema: schema}}))
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE") })
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+quoted+`.merchants (id,slug,status,permission_group_id) VALUES ($1,$2,'active',$3)`, merchantID.UUID(), w.slug, uuid.New())
	require.NoError(t, err)
	destination, err := db.NewWithPGXPool(w.pool, schema)
	require.NoError(t, err)
	result, err := merchantarchive.Restore(t.Context(), destination, merchantID, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err, "restore cause: %v", errors.Unwrap(err))
	require.Positive(t, result.Rows)

	rows := func(in, table, order string) string {
		t.Helper()
		var out string
		sql := fmt.Sprintf("SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY %s),'[]'::jsonb)::text FROM %s.%s t WHERE merchant_id=$1", order, in, pgx.Identifier{table}.Sanitize())
		require.NoError(t, w.pool.QueryRow(t.Context(), sql, merchantID.UUID()).Scan(&out))
		return out
	}
	original := pgx.Identifier{w.schema}.Sanitize()
	require.JSONEq(t, rows(original, "running_charges", "id"), rows(quoted, "running_charges", "id"), "both charges, exactly as they stood")
	var watched, due int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*), count(*) FILTER (WHERE next_check_at <= now()) FROM "+quoted+".balance_watches WHERE merchant_id=$1 AND customer_id=$2 AND currency='USD'",
		merchantID.UUID(), payer.cid().UUID()).Scan(&watched, &due))
	require.Equal(t, 1, watched, "the running charge is watched again")
	require.Equal(t, 1, due, "and due at once")

	// The same artifact again is a replay, not a second set of charges.
	replay, err := merchantarchive.Restore(t.Context(), destination, merchantID, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	require.True(t, replay.Replayed)
}
