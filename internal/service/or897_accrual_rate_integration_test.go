//go:build integration

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/delinquency"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// or#897 PR 3 — the cloud quota. "No more than $X/hour deployed at any instant"
// is not a spend window and not a credit line: it is a question about the RATE
// money would burn from now on, and only the host knows what it is about to
// start. So OpenRails measures what is already accruing and the host passes the
// prospective delta.

const or897Dollar = int64(1_000_000)

// The per-policy delinquency grace, proven where it is observable: two payers of
// the SAME merchant with the same debt of the same age transition differently,
// because they are bound to policies with different grace. Before or#897 that
// was impossible — grace was merchant-wide, so an enterprise tenant and a
// self-serve one had to be chased on the same clock.
func TestOr897_DelinquencyGraceIsReadFromTheBoundPolicy(t *testing.T) {
	svc, ms, _, ctx := wastedSvcEnv(t)
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()

	// Merchant-wide: 30 days of grace. Neither payer below would ever escalate
	// on it, so any escalation we see is the POLICY talking.
	wideGrace, floor := 30, int64(0)
	require.NoError(t, svc.SetMerchantConfiguration(ctx, billingservice.MerchantConfiguration{
		ArrearsGraceDays: &wideGrace, ArrearsDelinquencyFloor: &floor,
	}))

	strictGrace, lenientGrace, zeroFloor := 1, 90, int64(0)
	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "chase_fast", Kind: "outstanding_cap",
		DelinquencyGraceDays: &strictGrace, DelinquencyAmountFloor: &zeroFloor,
	}))
	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "chase_slow", Kind: "outstanding_cap",
		DelinquencyGraceDays: &lenientGrace, DelinquencyAmountFloor: &zeroFloor,
	}))
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{PolicyName: "chase_slow"}))

	slow := or897RatePayer(t, ctx, ms, pool)
	fast := or897RatePayer(t, ctx, ms, pool)
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{
		PolicyName: "chase_fast", CustomerID: openrails.CustomerID(fast.UUID()),
	}))

	// Identical debt, identical age: 10 days overdue.
	seedOverdueInvoice(t, ctx, pool, slow, money.DefaultCurrency, 5*or897Dollar, 10*24*time.Hour)
	seedOverdueInvoice(t, ctx, pool, fast, money.DefaultCurrency, 5*or897Dollar, 10*24*time.Hour)

	res, err := delinquency.NewService(dbi, nil).Evaluate(ctx, time.Now().UTC())
	require.NoError(t, err)

	states := map[uuid.UUID]delinquency.State{}
	for _, tr := range res.Transitions {
		states[tr.CustomerID] = tr.To
	}
	require.Equal(t, delinquency.StateDelinquent, states[fast.UUID()],
		"1 day of grace, 10 days overdue: the strictly-bound payer must be delinquent")
	require.Equal(t, delinquency.StateGrace, states[slow.UUID()],
		"90 days of grace on the same debt of the same age: still in grace")
}

// or897RatePayer seeds an arrears payer with a line big enough that
// affordability never binds — every verdict in these tests is the RATE talking.
func or897RatePayer(t *testing.T, ctx context.Context, ms *money.MoneyService, pool *pgxpool.Pool) identity.CustomerID {
	t.Helper()
	payer := or897ArrearsPayer(t, ctx, ms, pool)
	require.NoError(t, ms.SetCreditLimit(ctx, payer, money.DefaultCurrency, 100_000*or897Dollar))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM billing.usage_events WHERE customer_id = $1", payer.UUID())
	})
	return payer
}

// The per-policy COLLECTION TRIGGER, proven where it is observable: two payers
// of the same merchant with identical accrued arrears, one bound to a policy
// that invoices at $10 and one to a policy that invoices at $1000. The threshold
// pass must finalize exactly one of them.
//
// Before or#897 the trigger was merchant-wide, so a merchant could not bill a
// self-serve tenant monthly-ish and an enterprise one on a real credit line.
func TestOr897_CollectionThresholdIsReadFromTheBoundPolicy(t *testing.T) {
	svc, ms, _, ctx := wastedSvcEnv(t)
	pool := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()).Pool()

	// Merchant-wide trigger far above the debt below, so any invoice we see is
	// the POLICY talking, not the merchant default.
	wide := int64(1_000_000 * or897Dollar)
	require.NoError(t, svc.SetMerchantConfiguration(ctx, billingservice.MerchantConfiguration{
		InvoiceCollectionThreshold: &wide,
	}))

	eager, patient := int64(10*or897Dollar), int64(1000*or897Dollar)
	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "bill_eagerly", Kind: "outstanding_cap", CollectionThresholdAmount: &eager,
	}))
	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "bill_patiently", Kind: "outstanding_cap", CollectionThresholdAmount: &patient,
	}))
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{PolicyName: "bill_patiently"}))

	billed := or897RatePayer(t, ctx, ms, pool)
	unbilled := or897RatePayer(t, ctx, ms, pool)
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{
		PolicyName: "bill_eagerly", CustomerID: openrails.CustomerID(billed.UUID()),
	}))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM billing.invoices WHERE customer_id = ANY($1)",
			[]uuid.UUID{billed.UUID(), unbilled.UUID()})
	})

	// Identical debt, $50 each: over the eager trigger, well under the patient one.
	for _, p := range []identity.CustomerID{billed, unbilled} {
		_, err := ms.AccrueOwed(ctx, p, money.DefaultCurrency, "usage", "or897-threshold-"+p.UUID().String(), 50*or897Dollar)
		require.NoError(t, err)
	}

	// minThreshold 0 means "use each payer's resolved trigger" — the merchant-wide
	// value is already installed above and the policies override it per payer.
	settings, err := ms.InvoiceSettings(ctx)
	require.NoError(t, err)
	finalized, err := ms.FinalizeThresholdInvoices(ctx, time.Now().UTC().Add(time.Minute), money.InvoiceThresholdOptions{
		CollectionThresholdAmount: settings.CollectionThresholdAmount,
		BillingPeriodBoundary:     settings.BillingPeriodBoundary,
	})
	require.NoError(t, err)
	require.Equal(t, 1, finalized, "exactly the eagerly-bound payer is invoiced")

	require.Equal(t, 1, or897InvoiceCount(t, ctx, pool, billed), "the $10-trigger payer is billed at $50")
	require.Zero(t, or897InvoiceCount(t, ctx, pool, unbilled), "the $1000-trigger payer is not")
}

func or897InvoiceCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, payer identity.CustomerID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM billing.invoices WHERE customer_id = $1", payer.UUID()).Scan(&n))
	return n
}
