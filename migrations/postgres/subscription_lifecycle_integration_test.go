//go:build integration

package postgresmigrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// or#893 phase 2. The local lifecycle answers one question — "will we attempt
// to rebill this?" — and the enum carried two answers that are not answers to
// it: 'expired' (a clock reading, and as a TERMINAL state a direct
// contradiction of the NMI rebill doctrine, where a lapsed date is the normal
// state of every dunning customer) and 'failed' (a payment outcome, which has
// its own enum). 0078 removes both. This is the schema-level proof: they are
// not merely unwritten, they are unrepresentable.
func TestRetiredSubscriptionLifecycleValuesAreUnrepresentable(t *testing.T) {
	f := newPSPFixture(t)
	ctx := context.Background()

	labels := map[string]bool{}
	rows, err := f.pool.Query(ctx,
		`SELECT enumlabel FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid
		 JOIN pg_namespace n ON n.oid = t.typnamespace
		 WHERE n.nspname = 'openrails' AND t.typname = 'subscription_status'`)
	require.NoError(t, err)
	for rows.Next() {
		var l string
		require.NoError(t, rows.Scan(&l))
		labels[l] = true
	}
	rows.Close()
	require.NoError(t, rows.Err())

	require.Equal(t, map[string]bool{
		"pending": true, "active": true, "past_due": true, "cancelled": true, "unknown": true,
	}, labels, "the canonical local lifecycle is exactly these five states")

	for _, retired := range []string{"expired", "failed"} {
		err := f.insertSubscriptionWithStatus(t, retired)
		require.Error(t, err, "status %q must not be writable", retired)
		require.Contains(t, strings.ToLower(err.Error()), "invalid input value for enum",
			"status %q must fail at the TYPE, not at a check constraint that a later migration could relax", retired)
	}

	// The canonical set still works, including the terminal one — with the word
	// `expired` where it belongs, on cancel_type.
	require.NoError(t, f.insertSubscriptionWithStatus(t, "active"))
	require.NoError(t, f.insertSubscriptionWithStatus(t, "pending"))
	require.NoError(t, f.insertSubscriptionWithStatus(t, "unknown"))
	require.NoError(t, f.insertCancelledExpiredSubscription(t))
}

// The type swap dropped and rebuilt every index/CHECK/view compiled against the
// enum. A rebuild that quietly lost one would be invisible until a duplicate
// subscription or an unconstrained cancelled row appeared in production, so the
// load-bearing ones are asserted back.
func TestSubscriptionConstraintsSurvivedTheLifecycleTypeSwap(t *testing.T) {
	f := newPSPFixture(t)
	ctx := context.Background()

	for _, name := range []string{
		"chk_cancelled_has_timestamp",
		"chk_cancelled_has_type",
		"chk_cancelled_no_retry_schedule",
		"chk_past_due_has_period_end",
	} {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_constraint
			 WHERE conrelid = 'openrails.subscriptions'::regclass AND conname = $1`, name).Scan(&n))
		require.Equal(t, 1, n, "constraint %s must survive the enum swap", name)
	}
	var sst int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint
		 WHERE conrelid = 'openrails.subscription_status_transitions'::regclass
		   AND conname = 'chk_sst_real_transition'`).Scan(&sst))
	require.Equal(t, 1, sst, "#733's real-transition check constrains two enum columns and names no label — the swap must still carry it")

	for _, name := range []string{
		"idx_subscriptions_customer_active_created",
		"idx_subscriptions_due_dunning",
		"idx_subscriptions_period_overdue",
		"uq_subscriptions_customer_product_lifecycle",
		"uq_subscriptions_customer_tier_group_active",
		"ix_subscriptions_renewal_by_payment_method",
	} {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes WHERE schemaname = 'openrails' AND indexname = $1`, name).Scan(&n))
		require.Equal(t, 1, n, "index %s must survive the enum swap", name)
	}

	// The two analytics views read the enum and had to be dropped for the
	// retype; they are restored with their security_invoker setting and the
	// unprivileged role's SELECT, not just their bodies.
	for _, view := range []string{"freeloader_episodes", "orphaned_episodes"} {
		var invoker bool
		require.NoError(t, f.pool.QueryRow(ctx,
			`SELECT 'security_invoker=true' = ANY (c.reloptions)
			   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			  WHERE n.nspname = 'openrails' AND c.relname = $1`, view).Scan(&invoker))
		require.True(t, invoker, "%s must be restored WITH (security_invoker=true) — it reads merchant-scoped tables", view)

		var granted bool
		require.NoError(t, f.pool.QueryRow(ctx,
			`SELECT has_table_privilege('openrails_app', 'openrails.'||$1, 'SELECT')`, view).Scan(&granted))
		require.True(t, granted, "%s must be readable by openrails_app again", view)
	}

	// And the audit trigger, which pins the column and had to be dropped.
	var trig int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_trigger
		 WHERE tgrelid = 'openrails.subscriptions'::regclass
		   AND tgname = 'trg_subscriptions_status_transition' AND NOT tgisinternal`).Scan(&trig))
	require.Equal(t, 1, trig, "#733's status-transition audit trigger must be reattached")
}

// The transition audit itself must still record through the new type.
func TestStatusTransitionsAreStillAuditedThroughTheCanonicalType(t *testing.T) {
	f := newPSPFixture(t)
	ctx := context.Background()

	id := uuid.New()
	_, err := f.pool.Exec(ctx,
		`INSERT INTO openrails.subscriptions
		   (id, merchant_id, customer_id, product_id, price_id, status, rail, psp_id, started_at)
		 VALUES ($1, $2, $3, $4, $5, 'active', 'nmi', $6, now())`,
		id, f.merchant, f.customer, f.product, f.price, f.pspA)
	require.NoError(t, err)

	_, err = f.pool.Exec(ctx,
		`UPDATE openrails.subscriptions
		    SET status = 'cancelled', cancel_type = 'expired', cancelled_at = now(), ended_at = now()
		  WHERE id = $1`, id)
	require.NoError(t, err)

	var from, to string
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT from_status::text, to_status::text FROM openrails.subscription_status_transitions
		  WHERE subscription_id = $1 AND from_status IS NOT NULL`, id).Scan(&from, &to))
	require.Equal(t, "active", from)
	require.Equal(t, "cancelled", to)
}

// A fresh payer per row: uq_subscriptions_customer_product_lifecycle makes
// active/pending/past_due mutually exclusive per (merchant, customer, product),
// and that guarantee is not what these tests are probing.
func (f pspFixture) newCustomer(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
		id, f.merchant, uuid.NewString())
	require.NoError(t, err)
	return id
}

func (f pspFixture) insertSubscriptionWithStatus(t *testing.T, status string) error {
	t.Helper()
	// past_due needs a period end and cancelled needs its cancel columns; this
	// helper is for the states that need neither.
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO openrails.subscriptions
		   (id, merchant_id, customer_id, product_id, price_id, status, rail, psp_id, started_at)
		 VALUES ($1, $2, $3, $4, $5, $6::openrails.subscription_status, 'nmi', $7, now())`,
		uuid.New(), f.merchant, f.newCustomer(t), f.product, f.price, status, f.pspA)
	return err
}

func (f pspFixture) insertCancelledExpiredSubscription(t *testing.T) error {
	t.Helper()
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO openrails.subscriptions
		   (id, merchant_id, customer_id, product_id, price_id, status, cancel_type,
		    cancelled_at, ended_at, rail, psp_id, started_at)
		 VALUES ($1, $2, $3, $4, $5, 'cancelled', 'expired', now(), now(), 'nmi', $6, now())`,
		uuid.New(), f.merchant, f.newCustomer(t), f.product, f.price, f.pspA)
	return err
}
