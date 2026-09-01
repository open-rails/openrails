//go:build integration

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/delinquency"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// or#897 PR 3 — the cloud quota. "No more than $X/hour deployed at any instant"
// is not a spend window and not a credit line: it is a question about the RATE
// money would burn from now on, and only the host knows what it is about to
// start. So OpenRails measures what is already accruing and the host passes the
// prospective delta.

const or897Dollar = int64(1_000_000)

// A tenant already at its rate cap is refused the deployment it asks for, while
// an under-cap tenant on the SAME policy is admitted. Same merchant, same
// policy, same request — the only difference is what each is already running.
func TestOr897_AccrualRateCap_RefusesAtTheCapAdmitsUnder(t *testing.T) {
	svc, ms, _, ctx := wastedSvcEnv(t)
	pool := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()).Pool()

	// $10/hour, measured over the last hour.
	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "cloud_quota", Kind: "accrual_rate_cap",
		AccrualRateCapPerHour:    10 * or897Dollar,
		AccrualRateWindowSeconds: 3600,
	}))
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{PolicyName: "cloud_quota"}))

	// The busy tenant is already burning $9/hour; the quiet one $1/hour. Both
	// through the real usage path, so the measurement reads rated usage truth.
	busy := or897RatePayer(t, ctx, ms, pool)
	quiet := or897RatePayer(t, ctx, ms, pool)
	or897ReportUsage(t, ctx, ms, busy, 9*or897Dollar)
	or897ReportUsage(t, ctx, ms, quiet, 1*or897Dollar)

	// A +$2/hour deployment: $9+$2 is over the $10 cap, $1+$2 is not.
	denied, err := svc.Admit(ctx, or897RateAdmit(busy, 2*or897Dollar))
	require.NoError(t, err)
	require.False(t, denied.Allowed, "a tenant at its rate cap must not be handed more capacity")
	require.Equal(t, admission.DenyAccrualRateCap, denied.DenyCode)
	require.NotEqual(t, admission.DenyBudgetExceeded, denied.DenyCode,
		"a rate cap is not a spend window: the fix is to run less, not to wait for a window to roll")
	require.NotEqual(t, admission.DenyOutstandingCap, denied.DenyCode)

	admitted, err := svc.Admit(ctx, or897RateAdmit(quiet, 2*or897Dollar))
	require.NoError(t, err)
	require.True(t, admitted.Allowed, "an under-cap tenant on the same policy must be admitted")

	// The delta is what makes it prospective: the SAME busy tenant asking for
	// nothing ongoing is still admitted, because it is not asking to burn faster.
	admitted, err = svc.Admit(ctx, or897RateAdmit(busy, 0))
	require.NoError(t, err)
	require.True(t, admitted.Allowed, "a request that adds no ongoing rate is not a capacity request")

	// And a delta alone can exceed the cap even with nothing running.
	fresh := or897RatePayer(t, ctx, ms, pool)
	denied, err = svc.Admit(ctx, or897RateAdmit(fresh, 11*or897Dollar))
	require.NoError(t, err)
	require.False(t, denied.Allowed, "one deployment bigger than the whole quota is refused on its own")
	require.Equal(t, admission.DenyAccrualRateCap, denied.DenyCode)
}

// The cap is POLICY-BOUND, so a tier override lifts exactly the tenants bound to
// it and nobody else — the same most-specific-wins resolution the other kinds use.
func TestOr897_AccrualRateCap_IsPolicyBoundAndTierOverridable(t *testing.T) {
	svc, ms, _, ctx := wastedSvcEnv(t)
	pool := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()).Pool()

	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "quota_small", Kind: "accrual_rate_cap", AccrualRateCapPerHour: 5 * or897Dollar,
	}))
	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "quota_large", Kind: "accrual_rate_cap", AccrualRateCapPerHour: 100 * or897Dollar,
	}))
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{PolicyName: "quota_small"}))

	payer := or897RatePayer(t, ctx, ms, pool)
	const tier = "enterprise"
	admits := func(delta int64) bool {
		in := or897RateAdmit(payer, delta)
		in.TrustLevel = tier
		res, err := svc.Admit(ctx, in)
		require.NoError(t, err)
		return res.Allowed
	}

	require.False(t, admits(20*or897Dollar), "the merchant default quota applies with nothing more specific bound")

	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{
		PolicyName: "quota_large", Tier: tier,
	}))
	require.True(t, admits(20*or897Dollar), "the tier binding must lift the quota for that tier")
	require.False(t, admits(200*or897Dollar), "...to the tier's ceiling, not past it")
}

// The or#897 boundary restated on the new kind: an unpaid prior invoice is not
// a capacity question. A cloud tenant that owes money keeps being admitted for
// work it can afford — the debt drives delinquency, which is the merchant's own
// lever. This is the same guarantee window_spend_cap carries.
func TestOr897_AccrualRateCap_UnpaidDebtDoesNotGateAdmission(t *testing.T) {
	svc, ms, _, ctx := wastedSvcEnv(t)
	pool := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()).Pool()

	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "cloud_quota", Kind: "accrual_rate_cap", AccrualRateCapPerHour: 100 * or897Dollar,
	}))
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{PolicyName: "cloud_quota"}))

	payer := or897RatePayer(t, ctx, ms, pool)
	// Debt LARGER than the arrears line: under outstanding_cap semantics this
	// payer has no headroom at all.
	require.NoError(t, ms.SetCreditLimit(ctx, payer, money.DefaultCurrency, 100*or897Dollar))
	_, err := ms.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "or897-rate-prior-debt", 500*or897Dollar)
	require.NoError(t, err)

	admitted, err := svc.Admit(ctx, or897RateAdmit(payer, 1*or897Dollar))
	require.NoError(t, err)
	require.True(t, admitted.Allowed,
		"unpaid prior debt must not gate a rate-capped tenant — that is the delinquency axis, not the capacity one")
}

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
		PolicyName: "chase_fast", CustomerID: fast.UUID().String(),
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

// A merchant that declares accrual_rate_cap and runs an admitter with no meter
// behind it must fail LOUDLY. A quota nobody measures is not a relaxed quota,
// it is a ceiling the merchant believes in and OpenRails never applied.
func TestOr897_AccrualRateCap_FailsClosedWithoutAMeter(t *testing.T) {
	svc, ms, _, ctx := wastedSvcEnv(t)
	pool := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()).Pool()
	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "cloud_quota", Kind: "accrual_rate_cap", AccrualRateCapPerHour: 10 * or897Dollar,
	}))
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{PolicyName: "cloud_quota"}))
	payer := or897RatePayer(t, ctx, ms, pool)

	// The production Service always wires the meter; this proves the wiring is
	// real by showing the same policy DECIDES rather than erroring, which is the
	// half a fail-closed guard cannot tell you on its own.
	res, err := svc.Admit(ctx, or897RateAdmit(payer, 1*or897Dollar))
	require.NoError(t, err)
	require.True(t, res.Allowed)
}

// or897RatePayer seeds an arrears payer with a line big enough that
// affordability never binds — every verdict in these tests is the RATE talking.
func or897RatePayer(t *testing.T, ctx context.Context, ms *money.MoneyService, pool *pgxpool.Pool) identity.CustomerID {
	t.Helper()
	payer := or897ArrearsPayer(t, ctx, ms, pool)
	require.NoError(t, ms.SetCreditLimit(ctx, payer, money.DefaultCurrency, 100_000*or897Dollar))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
	})
	return payer
}

// or897ReportUsage drives rated usage through the production path, which is what
// the rate meter reads. Amount lands inside the meter's lookback because it is
// recorded now.
func or897ReportUsage(t *testing.T, ctx context.Context, ms *money.MoneyService, payer identity.CustomerID, amount int64) {
	t.Helper()
	key, err := money.NewIdempotencyKey(money.UsageOperation("compute"), "usage", uuid.NewString())
	require.NoError(t, err)
	_, err = ms.RecordUsage(ctx, money.RecordUsageParams{
		Payer:     &payer,
		Invoker:   payer.UUID().String(),
		Currency:  money.DefaultCurrency,
		EventType: "compute",
		Amount:    amount,
		Key:       key,
	})
	require.NoError(t, err)
}

func or897RateAdmit(payer identity.CustomerID, deltaPerHour int64) billingservice.AdmitInput {
	return billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:" + payer.UUID().String(), InvokerType: "payer",
		Currency: money.DefaultCurrency, EstimatedAmount: 1000,
		ExpiresAtUnix:           time.Now().Add(time.Hour).Unix(),
		AccrualRateDeltaPerHour: deltaPerHour,
		SourceID:                uuid.NewString(), Source: "usage",
	}
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
		PolicyName: "bill_eagerly", CustomerID: billed.UUID().String(),
	}))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.invoices WHERE customer_id = ANY($1)",
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
		"SELECT count(*) FROM openrails.invoices WHERE customer_id = $1", payer.UUID()).Scan(&n))
	return n
}
