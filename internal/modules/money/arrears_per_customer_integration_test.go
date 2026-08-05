//go:build integration

package money_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/pkg/identity"
)

// or#897: arrears liability is PER-DEBTOR, and outstanding owed is that
// account's negated balance — an O(1) counter read, not a sum over the payer's
// transfer history.

// The substrate itself: two payers accruing on the same merchant get separate
// liability accounts, each carrying only its own debt. Under the merchant-wide
// account this was unanswerable without scanning both payers' transfers.
func TestOr897_ArrearsIsPerCustomerAndOutstandingIsTheAccountBalance(t *testing.T) {
	svc, pool, payerA, cur, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	payerB := newArrearsPayer(t, ctx, pool)

	_, err := svc.AccrueOwed(ctx, payerA, cur, "usage", "or897-a1", 700_000)
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payerB, cur, "usage", "or897-b1", 250_000)
	require.NoError(t, err)

	owedA, err := svc.GetOutstandingOwed(ctx, payerA, cur)
	require.NoError(t, err)
	require.Equal(t, int64(700_000), owedA)
	owedB, err := svc.GetOutstandingOwed(ctx, payerB, cur)
	require.NoError(t, err)
	require.Equal(t, int64(250_000), owedB, "one payer's debt must not leak into another's exposure")

	// Two DISTINCT arrears accounts, and no merchant-wide one survives (hard cut).
	l := ledger.New(gen.New(pool), merchantID)
	accA, foundA, err := l.CustomerArrearsAccountID(ctx, payerA.UUID(), cur)
	require.NoError(t, err)
	require.True(t, foundA)
	accB, foundB, err := l.CustomerArrearsAccountID(ctx, payerB.UUID(), cur)
	require.NoError(t, err)
	require.True(t, foundB)
	require.NotEqual(t, accA, accB)

	var systemArrears int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.ledger_accounts
		 WHERE merchant_id = $1 AND account_type = 'arrears_liability' AND customer_id IS NULL
	`, merchantID).Scan(&systemArrears))
	require.Zero(t, systemArrears,
		"the merchant-wide arrears account is retired: two representations of one liability is the disease this removed")

	// The exposure is the account's negated balance, not a scan.
	balA, err := l.Balance(ctx, accA)
	require.NoError(t, err)
	require.Equal(t, int64(-700_000), balA, "debt is a NEGATIVE arrears balance")
}

// Conservation holds through the whole arrears lifecycle on per-customer
// accounts — the property the re-homing migration had to preserve. Runs the
// or#833 integrity checks (conservation + counter drift), so the new account
// shape is explicitly covered by the same audit the scheduled worker runs.
func TestOr897_PerCustomerArrearsPreservesLedgerIntegrity(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()

	_, err := svc.AccrueOwed(ctx, payer, cur, "usage", "or897-integrity-1", 900_000)
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "or897-integrity-2", 100_000)
	require.NoError(t, err)

	rep, err := ledger.CheckIntegrity(ctx, pool, merchantID)
	require.NoError(t, err)
	// Conservation is ledger-wide and must hold outright — that is the property
	// the re-homing had to preserve.
	require.Empty(t, rep.Conservation, "the re-homed ledger must stay conserved: %+v", rep.Conservation)
	// Counter drift is scoped to arrears accounts: this package's shared test
	// merchant also hosts fixtures that create drift DELIBERATELY (the or#833
	// trigger-bypass proof), so an unscoped assertion here would be measuring
	// those rather than this change.
	for _, d := range rep.Counters {
		require.NotEqual(t, "arrears_liability", d.AccountType,
			"a per-customer arrears account drifted from the transfer log: %+v", d)
	}

	owed, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(1_000_000), owed)
}

// A payer who never accrued has no arrears account, and reading their exposure
// must NOT create one (#534): an exposure read is a read.
func TestOr897_ExposureReadNeverCreatesAnAccount(t *testing.T) {
	svc, pool, _, cur, ctx := moneyInEnv(t)
	merchantID := dbtest.TestMerchantID.UUID()
	fresh := newArrearsPayer(t, ctx, pool)

	owed, err := svc.GetOutstandingOwed(ctx, fresh, cur)
	require.NoError(t, err)
	require.Zero(t, owed, "no arrears account = a clean zero, not an error")

	l := ledger.New(gen.New(pool), merchantID)
	_, found, err := l.CustomerArrearsAccountID(ctx, fresh.UUID(), cur)
	require.NoError(t, err)
	require.False(t, found, "a read must not have materialized an account")
}

func newArrearsPayer(t *testing.T, ctx context.Context, pool *pgxpool.Pool) identity.CustomerID {
	t.Helper()
	id := uuid.NewString()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, id)
	return identity.CustomerIDFromString(id)
}
