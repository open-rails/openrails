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
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchantarchive"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// A merchant archive carries running charges as they stand, a running one
// included, with the balance events the host acknowledged, and a restore
// watches every running one again: the watch is derived, not archived.
func TestRunningChargesSurviveAMerchantArchive(t *testing.T) {
	for _, at := range []struct {
		name          string
		sinceMidnight time.Duration
	}{
		{"after midnight", 2*time.Minute + 21*time.Second},
		{"daytime", 12 * time.Hour},
	} {
		t.Run(at.name, func(t *testing.T) { runningChargesSurviveArchiveAt(t, at.sinceMidnight) })
	}
}

func runningChargesSurviveArchiveAt(t *testing.T, sinceMidnight time.Duration) {
	t.Helper()
	w := prepareWorld(t, 12)
	now := w.clock.Now().UTC()
	w.clock.Advance(now.Truncate(24 * time.Hour).Add(sinceMidnight).Sub(now))
	w.start()
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

	// A customer already past its balance: the watcher tells the host, which
	// acknowledges the event before the cutover.
	broke := w.newCustomer()
	_, err = c.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{
		Reference: "archive-depleted", CustomerID: broke.cid(), Currency: "USD", RatePerHour: 3_600_000, StartedAt: &startedAt,
	})
	require.NoError(t, err)
	watcher := riverjobs.BalanceWatchWorker{DB: engine.Graph(w.rt).Runtime.DB, Money: engine.Graph(w.rt).Runtime.MoneyService}
	require.NoError(t, watcher.Work(t.Context(), &river.Job[riverjobs.BalanceWatchArgs]{}))
	events, err := c.ListHostEvents(t.Context(), billing.HostEventListParams{Type: billing.HostEventBalanceDepleted})
	require.NoError(t, err)
	require.Len(t, events.Items, 1)
	require.Equal(t, broke.cid(), events.Items[0].Balance.CustomerID)
	// Crossing midnight posts the preceding day's accrual to owed. The
	// customer's capacity still includes the whole hour on either side.
	balance := events.Items[0].Balance
	fmt.Printf("archive balance at %s: available=%d owed=%d\n", balance.AsOf.UTC().Format(time.RFC3339), balance.AvailableAmount, balance.OwedAmount)
	require.EqualValues(t, -3_600_000, balance.AvailableAmount-balance.OwedAmount)
	// The built-in 24-hour runway has told the host of the low balances too.
	low, err := c.ListHostEvents(t.Context(), billing.HostEventListParams{Type: billing.HostEventBalanceLow})
	require.NoError(t, err)
	acks := []billing.HostEventID{events.Items[0].ID}
	for _, e := range low.Items {
		acks = append(acks, e.ID)
	}
	_, err = c.AcknowledgeHostEvents(t.Context(), acks)
	require.NoError(t, err)
	// A prepaid balance with no running charge is watched as well.
	saver := w.newCustomer()
	_, err = c.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{
		{CustomerID: saver.cid(), Currency: "USD", Amount: 1_000_000, Source: "support", SourceID: "saver"},
	})
	require.NoError(t, err)

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
	require.JSONEq(t, rows(original, "running_charges", "id"), rows(quoted, "running_charges", "id"), "every charge, exactly as it stood")
	require.JSONEq(t, rows(original, "host_outbox", "id"), rows(quoted, "host_outbox", "id"), "the acknowledged balance event")
	var watched, due int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*), count(*) FILTER (WHERE next_check_at <= now()) FROM "+quoted+".balance_watches WHERE merchant_id=$1",
		merchantID.UUID()).Scan(&watched, &due))
	require.Equal(t, 3, watched, "each prepaid balance and running charge is watched again")
	require.Equal(t, 2, due, "a running charge's due at once")

	// The same artifact again is a replay, not a second set of charges.
	replay, err := merchantarchive.Restore(t.Context(), destination, merchantID, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	require.True(t, replay.Replayed)
}
