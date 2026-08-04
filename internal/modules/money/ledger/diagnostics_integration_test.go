//go:build integration

package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// #833: the diagnostics must CATCH a real drift, so this test manufactures one
// the only way it can actually happen in production — by BYPASSING the
// SECURITY DEFINER counter trigger (a superuser session, a COPY, a restore, a
// migration that disables triggers). A check that cannot detect that is worth
// nothing.
func TestLedgerDiagnostics_CatchTriggerBypassDrift(t *testing.T) {
	l, pool, ctx, customer, merchantID, cur := testLedger(t)

	_, err := l.Deposit(ctx, customer, cur, 1000, ledger.Coord{Operation: ledger.OpDeposit, Source: "grant", SourceID: uuid.NewString()}, uuid.New())
	require.NoError(t, err)
	custAcc, err := l.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)
	clearing, err := l.EnsureSystemAccount(ctx, ledger.RailClearing, cur)
	require.NoError(t, err)

	// Baseline: the honest path leaves both invariants intact.
	requireIntegrityClean(t, ctx, pool, merchantID, cur)

	// --- drift 1: a transfer that never reached the counters ------------------
	bypassTriggerInsert(t, ctx, pool, merchantID, clearing, custAcc, 250, cur)

	drifts := driftsForCurrency(t, ctx, pool, merchantID, cur)
	require.Len(t, drifts, 2, "both legs of the bypassed transfer must be reported")
	byAccount := map[uuid.UUID]ledger.CounterDrift{}
	for _, d := range drifts {
		byAccount[d.AccountID] = d
	}
	credited, ok := byAccount[custAcc]
	require.True(t, ok, "credited account must be reported")
	require.Equal(t, int64(1000), credited.StoredCredits)
	require.Equal(t, int64(1250), credited.LoggedCredits, "the log knows about the bypassed 250")
	debited, ok := byAccount[clearing]
	require.True(t, ok, "debited account must be reported")
	require.Equal(t, int64(1000), debited.StoredDebits)
	require.Equal(t, int64(1250), debited.LoggedDebits)

	// Conservation is deliberately BLIND to this one: the bypass skipped both
	// sides equally, so the balances still sum to zero. Documented in
	// diagnostics.go — it is exactly why the recompute exists.
	require.Empty(t, conservationForCurrency(t, ctx, pool, merchantID, cur),
		"a symmetric bypass does not break conservation; only the recompute sees it")

	// Repair the log and the diagnostics go quiet again — the check is precise,
	// not permanently red.
	_, err = ledgerOwnerPool(t).Exec(ctx,
		`DELETE FROM openrails.ledger_transfers WHERE merchant_id = $1 AND currency = $2 AND source = '833_test_bypass'`,
		merchantID, cur)
	require.NoError(t, err)
	requireIntegrityClean(t, ctx, pool, merchantID, cur)

	// --- drift 2: a one-sided counter corruption -----------------------------
	// A restore/COPY that rewrote one account's projection. Conservation is the
	// cheap check that catches this class.
	_, err = ledgerOwnerPool(t).Exec(ctx,
		`UPDATE openrails.ledger_accounts SET credits_posted = credits_posted + 777 WHERE id = $1`, custAcc)
	require.NoError(t, err)

	breaches := conservationForCurrency(t, ctx, pool, merchantID, cur)
	require.Len(t, breaches, 1, "the ledger no longer nets to zero")
	require.Equal(t, int64(777), breaches[0].Net)
	require.Equal(t, cur, breaches[0].Currency)
	require.Equal(t, merchantID, breaches[0].MerchantID)

	// The recompute independently names the account that was rewritten.
	drifts = driftsForCurrency(t, ctx, pool, merchantID, cur)
	require.Len(t, drifts, 1)
	require.Equal(t, custAcc, drifts[0].AccountID)
	require.Equal(t, int64(1777), drifts[0].StoredCredits)
	require.Equal(t, int64(1000), drifts[0].LoggedCredits)

	_, err = ledgerOwnerPool(t).Exec(ctx,
		`UPDATE openrails.ledger_accounts SET credits_posted = credits_posted - 777 WHERE id = $1`, custAcc)
	require.NoError(t, err)
	requireIntegrityClean(t, ctx, pool, merchantID, cur)
}

// bypassTriggerInsert appends a transfer with the counter trigger disabled for
// the duration of one transaction — the production failure mode, reproduced.
func bypassTriggerInsert(t *testing.T, ctx context.Context, _ *pgxpool.Pool, merchantID, debit, credit uuid.UUID, amount int64, cur string) {
	t.Helper()
	// Owner handle: DISABLE TRIGGER requires table ownership, which is the whole
	// point — this reproduces a superuser/restore path, not anything the app role
	// could ever do.
	tx, err := ledgerOwnerPool(t).Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	// ALTER TABLE is transactional, so the trigger can never stay disabled.
	_, err = tx.Exec(ctx, `ALTER TABLE openrails.ledger_transfers DISABLE TRIGGER trg_ledger_transfers_apply_counters`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `
INSERT INTO openrails.ledger_transfers
    (merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type, operation, source, source_id)
VALUES ($1, $2, $3, $4, $5, 'deposit', 'deposit', '833_test_bypass', gen_random_uuid()::text)`,
		merchantID, debit, credit, amount, cur)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `ALTER TABLE openrails.ledger_transfers ENABLE TRIGGER trg_ledger_transfers_apply_counters`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
}

// The diagnostics are merchant-scoped (an operator audits a merchant); the
// shared test DB holds other tests' ledgers, so assertions narrow to the
// test's own currency.
func conservationForCurrency(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID uuid.UUID, cur string) []ledger.ConservationBreach {
	t.Helper()
	all, err := ledger.CheckConservation(ctx, pool, merchantID)
	require.NoError(t, err)
	var out []ledger.ConservationBreach
	for _, b := range all {
		if b.Currency == cur {
			out = append(out, b)
		}
	}
	return out
}

func driftsForCurrency(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID uuid.UUID, cur string) []ledger.CounterDrift {
	t.Helper()
	all, err := ledger.CheckCounterDrift(ctx, pool, merchantID)
	require.NoError(t, err)
	var out []ledger.CounterDrift
	for _, d := range all {
		if d.Currency == cur {
			out = append(out, d)
		}
	}
	return out
}

func requireIntegrityClean(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID uuid.UUID, cur string) {
	t.Helper()
	require.Empty(t, conservationForCurrency(t, ctx, pool, merchantID, cur))
	require.Empty(t, driftsForCurrency(t, ctx, pool, merchantID, cur))
}

// CheckIntegrity composes both checks and OK() is the single operator verdict.
func TestLedgerDiagnostics_ReportComposesBothChecks(t *testing.T) {
	l, pool, ctx, customer, merchantID, cur := testLedger(t)
	_, err := l.Deposit(ctx, customer, cur, 500, ledger.Coord{Operation: ledger.OpDeposit, Source: "grant", SourceID: uuid.NewString()}, uuid.New())
	require.NoError(t, err)

	rep, err := ledger.CheckIntegrity(ctx, pool, merchantID)
	require.NoError(t, err)
	require.True(t, rep.OK(), "healthy ledger: %+v", rep)

	custAcc, err := l.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)
	_, err = ledgerOwnerPool(t).Exec(ctx,
		`UPDATE openrails.ledger_accounts SET debits_posted = debits_posted + 5 WHERE id = $1`, custAcc)
	require.NoError(t, err)

	rep, err = ledger.CheckIntegrity(ctx, pool, merchantID)
	require.NoError(t, err)
	require.False(t, rep.OK(), "a corrupted counter must fail the report")

	_, err = ledgerOwnerPool(t).Exec(ctx,
		`UPDATE openrails.ledger_accounts SET debits_posted = debits_posted - 5 WHERE id = $1`, custAcc)
	require.NoError(t, err)
}
