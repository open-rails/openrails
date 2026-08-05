//go:build integration

// Package postgresmigrations_test holds the DB-backed proofs for migration
// 0063. It is an EXTERNAL test package so it may import internal/dbtest (which
// runs the migrations) without a cycle.
package postgresmigrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

type pspFixture struct {
	pool     *pgxpool.Pool
	merchant uuid.UUID
	customer uuid.UUID
	price    uuid.UUID
	product  uuid.UUID
	pspA     uuid.UUID
	pspB     uuid.UUID
}

func newPSPFixture(t *testing.T) pspFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.SharedSuperuserPGXPool(t)
	f := pspFixture{pool: pool, merchant: uuid.New()}

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		f.merchant, "or893-"+uuid.NewString()[:8])
	f.pspA = dbtest.EnsureTestPSP(ctx, t, pool, f.merchant, "nmi")
	// A SECOND account on the same rail — the state 0017 exists for.
	f.pspB = uuid.New()
	exec(`INSERT INTO openrails.psps (id, merchant_id, rail, environment, account_id, key, archived)
	      VALUES ($1, $2, 'nmi', 'live', $3, 'paykings', false)`,
		f.pspB, f.merchant, "or893-b-"+uuid.NewString()[:8])

	f.customer = uuid.New()
	exec(`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
		f.customer, f.merchant, uuid.NewString())
	f.product, f.price = uuid.New(), uuid.New()
	exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id)
	      VALUES ($1, $2, $2, '{}'::jsonb, $3)`, f.product, "or893-p-"+uuid.NewString()[:8], f.merchant)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 1000000, 'USD', 720, true, $3)`, f.price, f.product, f.merchant)

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM openrails.merchants WHERE id = $1`, f.merchant)
	})
	return f
}

func (f pspFixture) insertPayment(t *testing.T, txnID, rail string, psp *uuid.UUID) error {
	t.Helper()
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO openrails.payments
		   (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount,
		    currency, status, purchased_at, psp_id, money_movement)
		 VALUES ($1, $2, $3, $4, $5, $6, 1000000, 1000000, 'USD', 'completed', now(), $7, 'rail')`,
		uuid.New(), f.merchant, f.customer, f.price, rail, txnID, psp)
	return err
}

// customer varies because uq_subscriptions_customer_product_lifecycle allows one
// live subscription per (customer, product) — an unrelated invariant that would
// otherwise mask the one under test.
func (f pspFixture) insertSubscription(t *testing.T, railSubID string, customer uuid.UUID, psp *uuid.UUID) error {
	t.Helper()
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO openrails.subscriptions
		   (id, merchant_id, customer_id, product_id, price_id, status, rail, rail_subscription_id,
		    started_at, entitlements_spec_snapshot, psp_id)
		 VALUES ($1, $2, $3, $4, $5, 'active', 'nmi', $6, now(), '{}'::jsonb, $7)`,
		uuid.New(), f.merchant, customer, f.product, f.price, railSubID, psp)
	return err
}

func (f pspFixture) newCustomer(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
		id, f.merchant, uuid.NewString())
	require.NoError(t, err)
	return id
}

// or#893, the defect PR #235 found and this migration closes.
//
// Before 0063 the payments unique keyed on COALESCE(psp_id, nil-uuid), so an
// unattributed row and its PSP-attributed twin hashed to two DIFFERENT keys and
// BOTH inserted: one provider charge, twice — double-counted revenue and double
// fulfilment. Run this test against the pre-0063 schema and the second INSERT
// below SUCCEEDS. Under 0063 the unattributed twin is refused outright, because
// payments_psp_required_on_rail makes an unattributed row on a real rail
// unrepresentable in the first place.
func TestAttributedAndUnattributedTwinsOfOneProviderTransactionCannotCoexist(t *testing.T) {
	f := newPSPFixture(t)

	require.NoError(t, f.insertPayment(t, "txn-twin", "nmi", &f.pspA),
		"the attributed row is the normal case")

	err := f.insertPayment(t, "txn-twin", "nmi", nil)
	require.Error(t, err, "the unattributed twin of an already-attributed provider transaction must be refused")
	require.Contains(t, strings.ToLower(err.Error()), "payments_psp_required_on_rail",
		"the refusal is the CHECK, not a unique violation: an unattributed row on a real rail never exists to collide")

	// Same transaction id under a DIFFERENT PSP is a DIFFERENT charge and stays
	// representable — that is why 0013's total (merchant, rail, txn) unique was
	// wrong and 0017 restored the PSP dimension.
	require.NoError(t, f.insertPayment(t, "txn-twin", "nmi", &f.pspB),
		"a provider id is only unique WITHIN a gateway account")

	// And the attributed lane is still exclusive within one PSP.
	require.Error(t, f.insertPayment(t, "txn-twin", "nmi", &f.pspA),
		"the same transaction cannot exist twice under one PSP")
}

func TestSubscriptionTwinsOfOneProviderSubscriptionCannotCoexist(t *testing.T) {
	f := newPSPFixture(t)

	require.NoError(t, f.insertSubscription(t, "sub-twin", f.customer, &f.pspA))
	err := f.insertSubscription(t, "sub-twin", f.newCustomer(t), nil)
	require.Error(t, err, "subscriptions.psp_id is NOT NULL: the unattributed lane does not exist")
	require.NoError(t, f.insertSubscription(t, "sub-twin", f.newCustomer(t), &f.pspB),
		"the same remote id under another PSP is another subscription")
	require.Error(t, f.insertSubscription(t, "sub-twin", f.newCustomer(t), &f.pspA),
		"the same remote id under one PSP is one subscription")
}

// Off-rail money is the one lane that legitimately carries no PSP, and it is
// classified explicitly rather than defaulted: a channel has no adapter, no
// credentials and no provider account, so there is nothing to attribute.
func TestOffRailChannelPaymentsMayCarryNoPSP(t *testing.T) {
	f := newPSPFixture(t)

	require.NoError(t, f.insertPayment(t, "manual-1", "manual", nil))
	require.NoError(t, f.insertPayment(t, "admin-1", "admin", nil))
	// Still deduped: the off-rail lane has its own total unique.
	require.Error(t, f.insertPayment(t, "manual-1", "manual", nil),
		"an off-rail payment id is still unique within (merchant, channel)")
}

// #704 restored, per or#893: two Stripe accounts hold INDEPENDENT customer
// mappings. Before this, the unique was (merchant, customer, rail) — whichever
// account's webhook landed last overwrote the other, and the reverse lookup then
// resolved the wrong account's cus_.
func TestRailCustomerAccountsAreScopedPerPSP(t *testing.T) {
	f := newPSPFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	upsert := func(psp uuid.UUID, accountID string) error {
		_, err := f.pool.Exec(ctx,
			`INSERT INTO openrails.rail_customer_accounts
			   (id, merchant_id, customer_id, rail, psp_id, account_id, created_at, updated_at)
			 VALUES ($1, $2, $3, 'nmi', $4, $5, $6, $6)
			 ON CONFLICT (merchant_id, customer_id, rail, psp_id) DO UPDATE
			   SET account_id = EXCLUDED.account_id, updated_at = EXCLUDED.updated_at`,
			uuid.New(), f.merchant, f.customer, psp, accountID, now)
		return err
	}

	require.NoError(t, upsert(f.pspA, "cus_a"))
	require.NoError(t, upsert(f.pspB, "cus_b"))

	var n int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.rail_customer_accounts WHERE merchant_id = $1 AND customer_id = $2`,
		f.merchant, f.customer).Scan(&n))
	require.Equal(t, 2, n, "one mapping per PSP; neither account overwrites the other")

	var got string
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT account_id FROM openrails.rail_customer_accounts
		  WHERE merchant_id = $1 AND customer_id = $2 AND rail = 'nmi' AND psp_id = $3`,
		f.merchant, f.customer, f.pspA).Scan(&got))
	require.Equal(t, "cus_a", got, "the reverse/forward lookup resolves the RIGHT account's customer")

	// The column is required: an unattributed mapping is unrepresentable.
	_, err := f.pool.Exec(ctx,
		`INSERT INTO openrails.rail_customer_accounts
		   (id, merchant_id, customer_id, rail, account_id, created_at, updated_at)
		 VALUES ($1, $2, $3, 'nmi', 'cus_x', $4, $4)`,
		uuid.New(), f.merchant, f.customer, now)
	require.Error(t, err, "rail_customer_accounts.psp_id is NOT NULL")
}

// The payment-method hole was the same shape as the payments one: two uniques
// disjoint on `psp_id IS NULL`, so one instrument was representable twice.
func TestPaymentMethodsCannotHoldAnUnattributedTwin(t *testing.T) {
	f := newPSPFixture(t)
	ctx := context.Background()

	insert := func(psp *uuid.UUID) error {
		_, err := f.pool.Exec(ctx,
			`INSERT INTO openrails.payment_methods
			   (id, merchant_id, customer_id, rail, rail_customer_ref, rail_method_ref, initial_transaction_id, psp_id)
			 VALUES ($1, $2, $3, 'nmi', 'vault-1', 'bill-1', '', $4)`,
			uuid.New(), f.merchant, f.customer, psp)
		return err
	}
	require.NoError(t, insert(&f.pspA))
	require.Error(t, insert(nil), "payment_methods.psp_id is NOT NULL")
	require.NoError(t, insert(&f.pspB), "the same vault ref under another PSP is another instrument")
	require.Error(t, insert(&f.pspA), "one instrument per (merchant, psp, vault ref, method ref)")
}
