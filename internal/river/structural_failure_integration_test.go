//go:build integration

package riverjobs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#901 end-to-end through a REAL River client: a job that failed on a
// structural precondition reaches a TERMINAL state with a typed reason on the
// first attempt, while a genuinely transient failure keeps every one of its
// retries. This is the defect measured on both standing stacks — 578 retryable
// openrails.catalog_reconciliation_pull rows, zero completions in 15 days, on
// two errors (merchant.ErrNoMerchant, then SQLSTATE 42883) that were never once
// going to succeed.

const (
	sfDriftKind     = "test.sf_schema_drift"
	sfNoMerchKind   = "test.sf_no_merchant"
	sfTransientKind = "test.sf_transient"
)

type sfDriftArgs struct{}

func (sfDriftArgs) Kind() string { return sfDriftKind }

type sfDriftWorker struct{ river.WorkerDefaults[sfDriftArgs] }

// The exact error the stacks returned once the or#877 fan-out shipped against a
// schema the or#893 re-squash could no longer reach.
func (sfDriftWorker) Work(context.Context, *river.Job[sfDriftArgs]) error {
	return fmt.Errorf("catalog reconciliation: list armed merchants: %w", &pgconn.PgError{
		Code:     "42883",
		Message:  "function openrails.psp_rail_merchant_ids(text[], integer) does not exist",
		Severity: "ERROR",
	})
}

type sfNoMerchArgs struct{}

func (sfNoMerchArgs) Kind() string { return sfNoMerchKind }

type sfNoMerchWorker struct{ river.WorkerDefaults[sfNoMerchArgs] }

// The exact error the stacks returned for the 15 days before that: the pull ran
// on a bare job context, so the rail resolver had no merchant to resolve.
func (sfNoMerchWorker) Work(ctx context.Context, _ *river.Job[sfNoMerchArgs]) error {
	if _, err := merchant.Require(ctx); err != nil {
		return fmt.Errorf("catalog reconciliation: resolve stripe rail: resolve rail stripe: %w", err)
	}
	return nil
}

type sfTransientArgs struct{}

func (sfTransientArgs) Kind() string { return sfTransientKind }

type sfTransientWorker struct{ river.WorkerDefaults[sfTransientArgs] }

// serialization_failure is the control: a Postgres error that IS transient and
// whose retries must survive the classifier untouched.
func (sfTransientWorker) Work(context.Context, *river.Job[sfTransientArgs]) error {
	return fmt.Errorf("write conflict: %w", &pgconn.PgError{
		Code:     "40001",
		Message:  "could not serialize access due to concurrent update",
		Severity: "ERROR",
	})
}

func sfJobRow(t *testing.T, dbi *db.DB, kind string) (state string, attempt int, errText string) {
	t.Helper()
	ctx := context.Background()
	err := dbi.Qx(ctx).QueryRow(ctx,
		`SELECT state::text, attempt, coalesce(errors::text, '') FROM river_job
		 WHERE kind = $1 ORDER BY id DESC LIMIT 1`, kind).
		Scan(&state, &attempt, &errText)
	require.NoError(t, err, "river_job row for %s", kind)
	return
}

func sfCleanup(t *testing.T, dbi *db.DB, kinds ...string) {
	t.Helper()
	ctx := context.Background()
	for _, k := range kinds {
		_, err := dbi.Qx(ctx).Exec(ctx, `DELETE FROM river_job WHERE kind = $1`, k)
		require.NoError(t, err)
	}
}

func TestStructuralFailure_TerminatesPreconditionFailuresAndKeepsTransientRetries(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbtest.SharedPGXPool(t)
	dbtest.EnsureTestMerchant(dbtest.WithTestMerchant(context.Background()), t, dbi.Pool())
	sfCleanup(t, dbi, sfDriftKind, sfNoMerchKind, sfTransientKind)
	t.Cleanup(func() { sfCleanup(t, dbi, sfDriftKind, sfNoMerchKind, sfTransientKind) })

	workers := river.NewWorkers()
	require.NoError(t, river.AddWorkerSafely(workers, &sfDriftWorker{}))
	require.NoError(t, river.AddWorkerSafely(workers, &sfNoMerchWorker{}))
	require.NoError(t, river.AddWorkerSafely(workers, &sfTransientWorker{}))

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}},
		Workers:           workers,
		Middleware:        []rivertype.Middleware{NewStructuralFailureMiddleware()},
		FetchCooldown:     10 * time.Millisecond,
		FetchPollInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)

	events, cancelSub := client.Subscribe(
		river.EventKindJobCompleted, river.EventKindJobFailed, river.EventKindJobCancelled)
	t.Cleanup(cancelSub)

	ctx := context.Background()
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = client.Stop(stopCtx)
	})

	// MaxAttempts deliberately > 1 everywhere: a terminal state must come from
	// CLASSIFICATION, never from exhausting retries.
	const maxAttempts = 5
	for _, args := range []river.JobArgs{sfDriftArgs{}, sfNoMerchArgs{}, sfTransientArgs{}} {
		_, err := client.Insert(ctx, args, &river.InsertOpts{MaxAttempts: maxAttempts})
		require.NoError(t, err)
	}
	awaitJobs(t, events, 3)

	// Schema drift: terminal on attempt 1, with the class named on the row.
	state, attempt, errText := sfJobRow(t, dbi, sfDriftKind)
	require.Equal(t, "cancelled", state, "42883 is deterministic; retrying it cannot help")
	require.Equal(t, 1, attempt, "cancelled by classification, not by exhaustion")
	require.Contains(t, errText, string(StructuralReasonSchemaDrift))
	require.Contains(t, errText, "psp_rail_merchant_ids", "the row keeps the underlying cause")

	// Missing merchant scope: same treatment.
	state, attempt, errText = sfJobRow(t, dbi, sfNoMerchKind)
	require.Equal(t, "cancelled", state)
	require.Equal(t, 1, attempt)
	require.Contains(t, errText, string(StructuralReasonNoMerchantScope))

	// Control: a transient error keeps its retries and never gets reclassified.
	// The job is either waiting for its backoff (retryable) or already promoted
	// by River's scheduler (available) — what matters is that it will run again.
	state, attempt, errText = sfJobRow(t, dbi, sfTransientKind)
	require.Contains(t, []string{"retryable", "available", "running"}, state,
		"serialization failures must still retry")
	require.Equal(t, 1, attempt)
	require.NotContains(t, errText, "structural precondition failure")
}
