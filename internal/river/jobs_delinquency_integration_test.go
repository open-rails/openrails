//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#878 + FC-16. The failure this test exists to prevent is the SILENT one: a
// scheduled worker that runs on the bare River job context reads an RLS-forced
// table as `merchant_id = NULL`, finds nothing, and reports success forever.
// Six workers shipped that way (or#862/or#868/or#877).
//
// So the assertion is not "the code compiles into a worker" — it is that ONE
// pass, driven only by a job on a bare context, discovers TWO different
// merchants' overdue payers through the SECURITY DEFINER work queue and records
// and signals both under their own scopes.
func TestDelinquencyWorker_EvaluatesEveryMerchantWithDueWork(t *testing.T) {
	ctx := context.Background()
	super := dbtest.SharedSuperuserPGXPool(t)
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))

	now := time.Now().UTC()
	due := now.Add(-30 * 24 * time.Hour)
	suffix := uuid.NewString()[:8]

	type seeded struct {
		merchant merchant.ID
		customer uuid.UUID
	}
	var fixtures []seeded
	for i, slug := range []string{"or878w-a-" + suffix, "or878w-b-" + suffix} {
		mid, cust := uuid.New(), uuid.New()
		_, err := super.Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, mid, slug)
		require.NoError(t, err)
		_, err = super.Exec(ctx,
			`INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2)`, cust, mid)
		require.NoError(t, err)
		_, err = super.Exec(ctx, `
			INSERT INTO openrails.invoices
				(merchant_id, customer_id, currency, period_from, period_to,
				 subtotal_amount, total_amount, amount_paid, amount_due, status, issued_at, due_at, finalized_at)
			VALUES ($1, $2, 'USD', $3, $4, $5, $5, 0, $5, 'open', $4, $4, $4)`,
			mid, cust, due.Add(-24*time.Hour), due, int64(5_000_000)*int64(i+1))
		require.NoError(t, err)
		// Grace 0 so the pass has a decision to make rather than a wait.
		_, err = super.Exec(ctx, `
			INSERT INTO openrails.merchant_configurations (merchant_id, config)
			VALUES ($1, '{"arrears_grace_days":0,"arrears_delinquency_floor":0}'::jsonb)`, mid)
		require.NoError(t, err)
		fixtures = append(fixtures, seeded{merchant: merchant.ID(mid), customer: cust})
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, f := range fixtures {
			_, _ = super.Exec(bg, `DELETE FROM openrails.host_lifecycle_events WHERE merchant_id = $1`, f.merchant.UUID())
			_, _ = super.Exec(bg, `DELETE FROM openrails.customer_delinquency WHERE merchant_id = $1`, f.merchant.UUID())
			_, _ = super.Exec(bg, `DELETE FROM openrails.notification_queue WHERE customer_id = $1`, f.customer)
			_, _ = super.Exec(bg, `DELETE FROM openrails.invoices WHERE merchant_id = $1`, f.merchant.UUID())
			_, _ = super.Exec(bg, `DELETE FROM openrails.merchant_configurations WHERE merchant_id = $1`, f.merchant.UUID())
			_, _ = super.Exec(bg, `DELETE FROM openrails.customers WHERE id = $1`, f.customer)
			_, _ = super.Exec(bg, `DELETE FROM openrails.merchants WHERE id = $1`, f.merchant.UUID())
		}
	})

	worker := DelinquencyWorker{DB: dbi, Clock: clockwork.NewFakeClockAt(now)}
	// A BARE context: no merchant, exactly as River hands it to the worker.
	require.NoError(t, worker.Work(context.Background(), &river.Job[DelinquencyArgs]{}))

	for _, f := range fixtures {
		mctx := merchant.WithID(context.Background(), f.merchant)
		require.NoError(t, dbi.RunInMerchantConn(mctx, func(sctx context.Context) error {
			q := dbi.Gen(sctx)
			rows, err := q.ListCustomerDelinquency(sctx, gen.ListCustomerDelinquencyParams{
				MerchantID: f.merchant.UUID(), CustomerID: f.customer,
			})
			require.NoError(t, err)
			require.Len(t, rows, 1, "merchant %s got no delinquency row — the pass was inert for it", f.merchant)
			require.Equal(t, "delinquent", rows[0].State)

			events, err := q.ListPendingHostLifecycleEvents(sctx, gen.ListPendingHostLifecycleEventsParams{
				MerchantID: f.merchant.UUID(), RowLimit: 10,
			})
			require.NoError(t, err)
			require.Len(t, events, 1, "the host must have been signalled once per merchant")
			require.Equal(t, "delinquency.entered", events[0].EventType)
			require.Equal(t, f.customer, events[0].SubjectID)
			return nil
		}))
	}

	// A second pass over unchanged state emits nothing new: the periodic
	// schedule must not re-instruct the host every 15 minutes.
	require.NoError(t, worker.Work(context.Background(), &river.Job[DelinquencyArgs]{}))
	for _, f := range fixtures {
		mctx := merchant.WithID(context.Background(), f.merchant)
		require.NoError(t, dbi.RunInMerchantConn(mctx, func(sctx context.Context) error {
			events, err := dbi.Gen(sctx).ListPendingHostLifecycleEvents(sctx, gen.ListPendingHostLifecycleEventsParams{
				MerchantID: f.merchant.UUID(), RowLimit: 10,
			})
			require.NoError(t, err)
			require.Len(t, events, 1, "a re-run of an unchanged state must not duplicate the signal")
			return nil
		}))
	}
}
