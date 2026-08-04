//go:build integration

package grants_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// Credits spend FIFO across lots (soonest expiry first); per-lot remaining is
// derived; over-spend is rejected atomically.
func TestGrants_CreditSpendFIFO(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + strings.ToUpper(short())
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
	require.Equal(t, int64(0), lotRemaining(t, ctx, pool, merchantID, customer, cur, lotA.ID))
	require.Equal(t, int64(350), lotRemaining(t, ctx, pool, merchantID, customer, cur, lotB.ID))

	// Over-spend the remainder is rejected atomically (nothing applied).
	require.ErrorIs(t, l.CreditSpend(ctx, customer, cur, 400, "invoker-1", "gpt", "spend", "req-2"), grants.ErrInsufficientCredits)
	mustBal(t, ctx, ml, custAcc, 350)

	requireConserved(t, ctx, pool, merchantID, cur)
}

// A lapsed credit lot's unspent remainder is clawed to expired_credits; idempotent.
func TestGrants_CreditExpire(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + strings.ToUpper(short())
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

	requireConserved(t, ctx, pool, merchantID, cur)
}

// #677: a spend and the lapsed-lot expiry serialize on the per-customer spend
// lock, so both can never consume the same lot. Lot B covers the ACCOUNT floor,
// so only per-lot accounting (not the overdraft trigger) can catch the
// over-consumption this guards against. The spender opens a tx, takes the lock
// (the money-path contract) and draws lot A while the expiry runs concurrently;
// ExpireLapsed must block on the lock and claw only the committed remainder.
func TestGrants_SpendExpiryConcurrency_LockSerializes(t *testing.T) {
	l, pool, ctx, customer, product, merchantID := testGrants(t)
	cur := "TC" + strings.ToUpper(short())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
	})

	edge := time.Now().UTC() // lot A lapses exactly here
	lateEnd := edge.Add(24 * time.Hour)
	lotA := mustCreditLot(t, ctx, l, customer, product, cur, 1000, edge.Add(-time.Hour), &edge)
	mustCreditLot(t, ctx, l, customer, product, cur, 1000, edge.Add(-time.Hour), &lateEnd)

	spendApplied := make(chan struct{})
	spendErr := make(chan error, 1)
	expireErr := make(chan error, 1)
	var expired int64

	// Spender: clock just BEFORE the edge (lot A still spendable). Lock + spend
	// 800 from lot A, hold the tx open while the expiry runs, then commit.
	go func() {
		spendErr <- func() error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			gl := grants.New(gen.New(tx), merchantID)
			gl.SetClock(func() time.Time { return edge.Add(-time.Second) })
			if err := gl.LockCustomer(ctx, customer); err != nil {
				return err
			}
			if err := gl.CreditSpend(ctx, customer, cur, 800, "invoker-1", "gpt", "spend", "race-1"); err != nil {
				return err
			}
			close(spendApplied)
			time.Sleep(300 * time.Millisecond) // hold the lock while the expiry contends
			return tx.Commit(ctx)
		}()
	}()

	// Expirer: the credit-expiry job path — ExpireLapsed in its own tx, clock
	// just PAST the edge (lot A lapsed). It must block until the spend commits.
	go func() {
		expireErr <- func() error {
			<-spendApplied
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			gl := grants.New(gen.New(tx), merchantID)
			gl.SetClock(func() time.Time { return edge.Add(time.Second) })
			var e error
			if expired, e = gl.ExpireLapsed(ctx, customer, cur); e != nil {
				return e
			}
			return tx.Commit(ctx)
		}()
	}()

	require.NoError(t, <-spendErr)
	require.NoError(t, <-expireErr)
	require.Equal(t, int64(200), expired, "expiry claws only the post-spend remainder")

	// Lot A consumed exactly once: spend 800 + expire 200 == the 1000 deposited.
	var consumed int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount),0)::bigint FROM openrails.ledger_transfers
		 WHERE merchant_id=$1 AND grant_id=$2 AND transfer_type IN ('credit_spend','credit_expire')`,
		merchantID, lotA.ID).Scan(&consumed))
	require.Equal(t, int64(1000), consumed, "lot A must not be over-consumed")
	requireConserved(t, ctx, pool, merchantID, cur)
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

func lotRemaining(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID, customer uuid.UUID, cur string, lotID uuid.UUID) int64 {
	t.Helper()
	lots, err := gen.New(pool).ListSpendableCreditLots(ctx, gen.ListSpendableCreditLotsParams{
		MerchantID: merchantID, CustomerID: customer, Currency: cur, AsOf: time.Now().UTC(),
	})
	require.NoError(t, err)
	for _, lot := range lots {
		if lot.ID == lotID {
			return lot.Remaining
		}
	}
	t.Fatalf("lot %s not found among spendable lots", lotID)
	return 0
}
