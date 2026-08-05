//go:build integration

package riverjobs

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// or#833: the checks existed and were proven to catch a real drift; NOTHING RAN
// THEM. This proves the JOB runs them — end to end on a real ledger, under the
// RLS-enforcing role, with the drift induced the only way it happens in
// production (a write that bypassed the counter trigger).
func TestLedgerIntegrityWorker_RaisesAFindingOnInducedDrift(t *testing.T) {
	ctx := context.Background()
	merchantID := dbtest.TestMerchantID.UUID()
	merchantPool := dbtest.SharedMerchantPool(t, merchantID)
	dbtest.EnsureTestMerchant(ctx, t, merchantPool)
	mctx := dbtest.WithTestMerchant(ctx)

	// The WORKER's handle is UNPINNED — production's posture. Everything it can
	// see, it sees because RunInMerchantScope pinned the merchant (or#824: the
	// ledger tables FORCE RLS, so a bare-context sweep reads nothing and reports
	// a clean fleet).
	workerDB := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	worker := LedgerIntegrityWorker{DB: workerDB, Clock: clockwork.NewRealClock()}

	// A real ledger: one deposit, through the real Ledger API.
	currency := "TL" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:10])
	customer := uuid.New()
	_, err := merchantPool.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		customer, merchantID)
	require.NoError(t, err)
	l := ledger.New(gen.New(merchantPool), merchantID)
	_, err = l.Deposit(mctx, customer, currency, 1000, ledger.Coord{Operation: ledger.OpDeposit, Source: "grant", SourceID: uuid.NewString()}, uuid.New())
	require.NoError(t, err)
	custAcc, err := l.EnsureCustomerBalance(mctx, customer, currency)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = merchantPool.Exec(ctx, `DELETE FROM openrails.ledger_transfers WHERE merchant_id=$1 AND currency=$2`, merchantID, currency)
		_, _ = merchantPool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, currency)
		_, _ = merchantPool.Exec(ctx, `DELETE FROM openrails.reconciliation_findings
			WHERE merchant_id=$1 AND (subject_key=$2 OR subject_key=$3)`,
			merchantID, custAcc.String(), strings.ToLower(currency))
	})

	// A healthy ledger is silent.
	require.NoError(t, worker.Work(ctx, nil))
	require.Zero(t, findingStatus(t, ctx, merchantPool, merchantID, FindingLedgerCounterDrift, custAcc.String()).count,
		"a clean ledger must raise nothing")

	// The drift: a one-sided counter rewrite — a restore, a COPY, a migration
	// with triggers disabled. Silent by construction: no error, no event, and
	// every balance read on this account is wrong from here on.
	_, err = superuserPool(t).Exec(ctx,
		`UPDATE openrails.ledger_accounts SET credits_posted = credits_posted + 777 WHERE id = $1`, custAcc)
	require.NoError(t, err)

	require.NoError(t, worker.Work(ctx, nil))

	drift := findingStatus(t, ctx, merchantPool, merchantID, FindingLedgerCounterDrift, custAcc.String())
	require.Equal(t, 1, drift.count, "the scheduled audit did not notice a drifted account counter")
	require.Equal(t, "critical", drift.severity)
	require.Equal(t, "requires_review", drift.status)
	require.Contains(t, drift.action, "bypassed the trigger")

	// The one-sided rewrite also breaks conservation — the cheap check — and the
	// job files that separately, keyed on the currency.
	cons := findingStatus(t, ctx, merchantPool, merchantID, FindingLedgerConservation, strings.ToLower(currency))
	require.Equal(t, 1, cons.count, "the ledger no longer nets to zero and nobody said so")
	require.Equal(t, "critical", cons.severity)

	// Repaired: the audit is precise, not permanently red.
	_, err = superuserPool(t).Exec(ctx,
		`UPDATE openrails.ledger_accounts SET credits_posted = credits_posted - 777 WHERE id = $1`, custAcc)
	require.NoError(t, err)
	require.NoError(t, worker.Work(ctx, nil))
	require.Equal(t, "fixed", findingStatus(t, ctx, merchantPool, merchantID, FindingLedgerCounterDrift, custAcc.String()).status)
	require.Equal(t, "fixed", findingStatus(t, ctx, merchantPool, merchantID, FindingLedgerConservation, strings.ToLower(currency)).status)
}

type ledgerFinding struct {
	count    int
	severity string
	status   string
	action   string
}

func findingStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID uuid.UUID, findingType, subject string) ledgerFinding {
	t.Helper()
	var f ledgerFinding
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*), coalesce(max(severity),''), coalesce(max(status),''), coalesce(max(recommended_action),'')
		   FROM openrails.reconciliation_findings
		  WHERE merchant_id=$1 AND finding_type=$2 AND subject_key=$3`,
		merchantID, findingType, subject).Scan(&f.count, &f.severity, &f.status, &f.action))
	return f
}

// superuserPool is the privileged handle used ONLY to manufacture the drift —
// which is the point: only a session that bypasses the app role's constraints
// can produce this fault, and that is exactly why nothing else can see it.
func superuserPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	superDSN, _ := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), superDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}
