//go:build integration

package grants_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// Revoking a credit grant claws back its UNSPENT remainder to revoked_credits:
// the lot becomes unspendable, the derived balance drops by the unspent amount,
// the money is frozen (not refunded), conservation holds, and re-deriving is
// idempotent. (#514, docs/consistency-invariants.md §11 decision 4.)
func TestGrants_RevokeClawback(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + short()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
	})
	ml := ledger.New(gen.New(pool), merchantID)
	custAcc, err := ml.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)

	lot := mustCreditLot(t, ctx, l, customer, product, cur, 100, time.Now().UTC(), nil)
	require.NoError(t, l.CreditSpend(ctx, customer, cur, 30, "inv", "gpt", "spend", "req-1"))
	mustBal(t, ctx, ml, custAcc, 70)

	clawed, err := l.RevokeGrant(ctx, lot.ID, "admin removed", false)
	require.NoError(t, err)
	require.Equal(t, int64(70), clawed, "only the unspent remainder is clawed")

	// The revoked lot is no longer spendable…
	require.False(t, lotSpendable(t, ctx, l, customer, cur, lot.ID))
	// …the derived balance drops to 0 (the 70 retracted)…
	mustBal(t, ctx, ml, custAcc, 0)
	// …the money is frozen in revoked_credits (NOT refunded)…
	require.Equal(t, int64(70), sysBal(t, ctx, ml, ledger.RevokedCredits, cur))
	// …the spent 30 stays as revenue…
	require.Equal(t, int64(30), sysBal(t, ctx, ml, ledger.PlatformRevenue, cur))
	requireConserved(t, ctx, ml, cur)

	// Idempotent: re-deriving the revoked grant claws nothing more.
	require.NoError(t, l.MaterializeGrant(ctx, lot))
	mustBal(t, ctx, ml, custAcc, 0)
	require.Equal(t, int64(70), sysBal(t, ctx, ml, ledger.RevokedCredits, cur))
	requireConserved(t, ctx, ml, cur)
}

// The clawback is a pure reversing transfer, so it is reversible at the ledger
// level: returning the frozen remainder to customer_balance restores the balance
// and conservation — no value was destroyed.
func TestGrants_RevokeClawbackReversible(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + short()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
	})
	ml := ledger.New(gen.New(pool), merchantID)
	custAcc, err := ml.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)

	lot := mustCreditLot(t, ctx, l, customer, product, cur, 100, time.Now().UTC(), nil)
	clawed, err := l.RevokeGrant(ctx, lot.ID, "mistake", false)
	require.NoError(t, err)
	require.Equal(t, int64(100), clawed)
	mustBal(t, ctx, ml, custAcc, 0)
	require.Equal(t, int64(100), sysBal(t, ctx, ml, ledger.RevokedCredits, cur))

	// Reverse the clawback: revoked_credits -> customer_balance.
	revAcc, err := ml.EnsureSystemAccount(ctx, ledger.RevokedCredits, cur)
	require.NoError(t, err)
	src, sid, c := "grant_reinstate", lot.ID.String(), customer
	_, err = ml.Apply(ctx, ledger.Transfer{
		Debit: revAcc, Credit: custAcc, Amount: 100, Currency: cur, Type: "credit_reinstate",
		Source: &src, SourceID: &sid, GrantID: &lot.ID, Customer: &c,
	})
	require.NoError(t, err)
	mustBal(t, ctx, ml, custAcc, 100) // restored
	require.Equal(t, int64(0), sysBal(t, ctx, ml, ledger.RevokedCredits, cur))
	requireConserved(t, ctx, ml, cur)
}

// RevokeGrant with refund=true also moves the clawed-back amount out of the
// platform (revoked_credits -> processor_clearing), netting the deposit so the
// money has truly left; the customer balance is 0 and the books conserve.
func TestGrants_RevokeWithRefund(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + short()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
	})
	ml := ledger.New(gen.New(pool), merchantID)
	custAcc, err := ml.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)

	lot := mustCreditLot(t, ctx, l, customer, product, cur, 100, time.Now().UTC(), nil)
	mustBal(t, ctx, ml, custAcc, 100)
	// processor_clearing = -100 after the deposit.
	require.Equal(t, int64(-100), sysBal(t, ctx, ml, ledger.RailClearing, cur))

	clawed, err := l.RevokeGrant(ctx, lot.ID, "refunded", true)
	require.NoError(t, err)
	require.Equal(t, int64(100), clawed)

	mustBal(t, ctx, ml, custAcc, 0)
	require.Equal(t, int64(0), sysBal(t, ctx, ml, ledger.RevokedCredits, cur), "refund drains the frozen credits out")
	require.Equal(t, int64(0), sysBal(t, ctx, ml, ledger.RailClearing, cur), "money returned: deposit -100 + refund +100")
	requireConserved(t, ctx, ml, cur)
}

// --- helpers ---

func sysBal(t *testing.T, ctx context.Context, ml *ledger.Ledger, at ledger.AccountType, cur string) int64 {
	t.Helper()
	acc, err := ml.EnsureSystemAccount(ctx, at, cur)
	require.NoError(t, err)
	b, err := ml.Balance(ctx, acc)
	require.NoError(t, err)
	return b
}

func requireConserved(t *testing.T, ctx context.Context, ml *ledger.Ledger, cur string) {
	t.Helper()
	net, err := ml.Conservation(ctx, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), net, "ledger conserves (Σ == 0)")
}

func lotSpendable(t *testing.T, ctx context.Context, l *grants.Ledger, customer uuid.UUID, cur string, lotID uuid.UUID) bool {
	t.Helper()
	lots, err := l.SpendableLots(ctx, customer, cur)
	require.NoError(t, err)
	for _, lot := range lots {
		if lot.ID == lotID {
			return true
		}
	}
	return false
}
