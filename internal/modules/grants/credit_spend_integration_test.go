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

// Credits spend FIFO across lots (soonest expiry first); per-lot remaining is
// derived; over-spend is rejected atomically.
func TestGrants_CreditSpendFIFO(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + short()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
	})

	now := time.Now().UTC()
	soonEnd := now.Add(1 * time.Hour)
	lateEnd := now.Add(24 * time.Hour)
	lotA := mustCreditLot(t, ctx, l, customer, product, cur, 100, now, &soonEnd) // expires sooner
	lotB := mustCreditLot(t, ctx, l, customer, product, cur, 500, now, &lateEnd) // expires later

	ml := ledger.New(gen.New(pool), merchantID)
	custAcc, err := ml.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)
	mustBal(t, ctx, ml, custAcc, 600) // both deposited

	// Spend 250 -> drains lotA (100) then 150 of lotB (FIFO by expiry).
	require.NoError(t, l.CreditSpend(ctx, customer, cur, 250, "invoker-1", "gpt", "spend", "req-1"))
	mustBal(t, ctx, ml, custAcc, 350)
	require.Equal(t, int64(0), lotRemaining(t, ctx, l, customer, cur, lotA.ID))
	require.Equal(t, int64(350), lotRemaining(t, ctx, l, customer, cur, lotB.ID))

	// Over-spend the remainder is rejected atomically (nothing applied).
	require.ErrorIs(t, l.CreditSpend(ctx, customer, cur, 400, "invoker-1", "gpt", "spend", "req-2"), grants.ErrInsufficientCredits)
	mustBal(t, ctx, ml, custAcc, 350)

	net, err := ml.Conservation(ctx, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), net)
}

// A lapsed credit lot's unspent remainder is clawed to expired_credits; idempotent.
func TestGrants_CreditExpire(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + short()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
	})

	now := time.Now().UTC()
	start := now.Add(-2 * time.Hour)
	end := now.Add(-1 * time.Hour)
	mustCreditLot(t, ctx, l, customer, product, cur, 100, start, &end) // already lapsed

	ml := ledger.New(gen.New(pool), merchantID)
	custAcc, err := ml.EnsureCustomerBalance(ctx, customer, cur)
	require.NoError(t, err)
	mustBal(t, ctx, ml, custAcc, 100)

	expired, err := l.ExpireLapsed(ctx, customer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(100), expired)
	mustBal(t, ctx, ml, custAcc, 0)

	expAcc, err := ml.EnsureSystemAccount(ctx, ledger.ExpiredCredits, cur)
	require.NoError(t, err)
	mustBal(t, ctx, ml, expAcc, 100)

	// Idempotent — nothing left to expire.
	again, err := l.ExpireLapsed(ctx, customer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), again)
	mustBal(t, ctx, ml, custAcc, 0)

	net, err := ml.Conservation(ctx, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), net)
}

func mustCreditLot(t *testing.T, ctx context.Context, l *grants.Ledger, customer, product uuid.UUID, cur string, amount int64, start time.Time, end *time.Time) gen.OpenrailsGrant {
	t.Helper()
	g, err := l.Grant(ctx, grants.GrantInput{
		Customer: customer, Product: &product, Kind: grants.Credit, Source: grants.Purchase,
		SourceID: "pay_" + short(), Amount: &amount, Currency: &cur, StartsAt: start, EndsAt: end,
	})
	require.NoError(t, err)
	require.NoError(t, l.MaterializeGrant(ctx, g)) // deposit into the #512 ledger
	return g
}

func mustBal(t *testing.T, ctx context.Context, ml *ledger.Ledger, acc uuid.UUID, want int64) {
	t.Helper()
	got, err := ml.Balance(ctx, acc)
	require.NoError(t, err)
	require.Equal(t, want, got, "balance of %s", acc)
}

func lotRemaining(t *testing.T, ctx context.Context, l *grants.Ledger, customer uuid.UUID, cur string, lotID uuid.UUID) int64 {
	t.Helper()
	lots, err := l.SpendableLots(ctx, customer, cur)
	require.NoError(t, err)
	for _, lot := range lots {
		if lot.ID == lotID {
			return lot.Remaining
		}
	}
	t.Fatalf("lot %s not found among spendable lots", lotID)
	return 0
}
