//go:build integration

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/delinquency"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// or#897 PR 2: the two seed businesses, driven through the production entry
// points (SetBillingPolicy / BindBillingPolicy → Admit), differ in exactly one
// thing — WHICH quantity their bound policy caps. Everything else about them is
// identical, which is the point of a registry rather than two code paths.

// declineCharger declines every collection attempt with one verbatim rail code.
// "05" is a plain do-not-honor: or#870 bucket 1 (retryable), so the receivable
// stays open and the debt stands. That is the realistic case — a credit line
// exists precisely because collection can fail.
type declineCharger struct {
	attempts int
}

func (c *declineCharger) ChargeSavedMethod(_ context.Context, _ money.ChargeRequest) (money.ChargeResult, error) {
	c.attempts++
	code, message := "05", "do not honor"
	return money.ChargeResult{Rail: "nmi", Declined: true, FailureCode: &code, FailureMessage: &message}, nil
}

// SCENARIO 1 — the API business. A $200 credit line on OUTSTANDING owed.
// $155 accrued and uncollected leaves EXACTLY $45 of headroom; the request that
// crosses it is refused with outstanding_cap_reached — not insufficient_credit
// ("this one request does not fit") and not delinquent (the time axis, and this
// debt is not even due yet).
func TestOr897_OutstandingCapPolicy_SeedAPIBusiness(t *testing.T) {
	svc, ms, _, ctx := wastedSvcEnv(t)
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()

	const line = int64(200_000_000) // $200
	const owed = int64(155_000_000) // $155 accrued
	const headroom = line - owed    // $45 — the whole assertion
	const bite = int64(1_000_000)   // $1, the request that must not fit

	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "api_line", Kind: "outstanding_cap", OutstandingCapAmount: line,
	}))
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{PolicyName: "api_line"}))

	payer := or897ArrearsPayer(t, ctx, ms, pool)
	method := or897SeedPaymentMethod(t, ctx, pool, payer)
	_, err := ms.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		AutoTopupPaymentMethod: &method,
	})
	require.NoError(t, err)

	// The debt through the REAL accrual path: an owed_accrual leg on the payer's
	// own arrears account, which is what the cap measures.
	_, err = ms.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "or897-seed-api", owed)
	require.NoError(t, err)

	// Collection is attempted and FAILS (bucket 1). The debt is unchanged, which
	// is the state the credit line has to hold the payer to.
	invoice, err := ms.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().UTC().Add(-24*time.Hour), time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, "open", invoice.Status)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	charger := &declineCharger{}
	collected, err := ms.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)
	require.Zero(t, collected, "the charge declined, so nothing was collected")
	require.Positive(t, charger.attempts, "collection must actually have been attempted")

	exposure, err := ms.GetOutstandingOwed(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.EqualValues(t, owed, exposure, "a failed collection leaves the debt standing")

	// EXACTLY $45 more is admitted...
	admitted, err := svc.Admit(ctx, or897Admit(payer, headroom))
	require.NoError(t, err)
	require.True(t, admitted.Allowed, "the $45 that remains under the $200 line must be admitted")

	// ...and the debt is now at the line, so nothing more may be spent on credit.
	// The hold above already consumed the headroom in Redis; accrue it so the
	// LEDGER agrees and the refusal is measured, not merely reserved.
	_, err = ms.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "or897-seed-api-headroom", headroom)
	require.NoError(t, err)

	denied, err := svc.Admit(ctx, or897Admit(payer, bite))
	require.NoError(t, err)
	require.False(t, denied.Allowed, "at the cap, new spend on credit is refused")
	require.Equal(t, admission.DenyOutstandingCap, denied.DenyCode,
		"the host must be able to tell 'your debt is at the cap, pay it down' from 'you are out of balance'")
	require.NotEqual(t, money.DenyInsufficientCredit, denied.DenyCode)
	require.NotEqual(t, admission.DenyDelinquent, denied.DenyCode,
		"the debt is not even overdue; this is the amount axis, not the time axis")
}

// SCENARIO 2 — the cloud business. A $2k/month cap on NEW spend. An unpaid
// prior invoice does NOT gate admission — the window still does, and the debt
// feeds the delinquency signal the merchant acts on itself.
func TestOr897_WindowSpendCapPolicy_SeedCloudBusiness(t *testing.T) {
	svc, ms, _, ctx := wastedSvcEnv(t)
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()

	const window = int64(2_000_000_000)    // $2k/month — the cap that IS enforced
	const line = int64(2_500_000_000)      // the payer's arrears line
	const priorDebt = int64(3_000_000_000) // MORE than the line: outstanding_cap would refuse outright

	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "cloud_monthly", Kind: "window_spend_cap",
		SpendWindows: []billingservice.SpendLimitWindowInput{
			{Key: "monthly", WindowSeconds: 30 * 24 * 3600, Limit: window},
		},
	}))
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{PolicyName: "cloud_monthly"}))

	payer := or897ArrearsPayer(t, ctx, ms, pool)
	// The prior debt EXCEEDS the arrears line. Under outstanding_cap semantics
	// this payer would have zero headroom and be refused outright; under
	// window_spend_cap the debt must not touch admission at all.
	require.NoError(t, ms.SetCreditLimit(ctx, payer, money.DefaultCurrency, line))
	_, err := ms.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "or897-seed-cloud-prior", priorDebt)
	require.NoError(t, err)

	exposure, err := ms.GetOutstandingOwed(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.EqualValues(t, priorDebt, exposure)

	admitted, err := svc.Admit(ctx, or897Admit(payer, 10_000_000))
	require.NoError(t, err)
	require.True(t, admitted.Allowed,
		"an unpaid prior invoice must NOT gate a window_spend_cap payer — that debt drives delinquency, not admission")

	// The window still bites: the request that carries the month past $2k is
	// refused, with the WINDOW's code, not the debt's. It is comfortably inside
	// the arrears line, so this is the window talking and nothing else.
	denied, err := svc.Admit(ctx, or897Admit(payer, window))
	require.NoError(t, err)
	require.False(t, denied.Allowed, "the monthly window is still a real cap")
	require.Equal(t, admission.DenyBudgetExceeded, denied.DenyCode)
	require.NotEqual(t, admission.DenyOutstandingCap, denied.DenyCode,
		"a window_spend_cap payer can never hit the outstanding cap — it has none")

	// And the debt is not ignored: it feeds the delinquency signal, which is the
	// merchant's own lever (OpenRails turns nothing off).
	grace, floor := 0, int64(0)
	require.NoError(t, svc.SetMerchantConfiguration(ctx, billingservice.MerchantConfiguration{
		ArrearsGraceDays: &grace, ArrearsDelinquencyFloor: &floor,
	}))
	seedOverdueInvoice(t, ctx, pool, payer, money.DefaultCurrency, priorDebt, 30*24*time.Hour)
	res, err := delinquency.NewService(dbi, nil).Evaluate(ctx, time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, res.Transitions, 1, "the unpaid debt must raise the delinquency signal")
	require.Equal(t, delinquency.StateDelinquent, res.Transitions[0].To)
}

// Most specific wins: a payer with no binding of its own follows the merchant
// default, then its tier's binding, then its own. Each rung is proven by the
// ADMISSION VERDICT changing, not by reading the row back.
func TestOr897_BindingResolutionPrecedence(t *testing.T) {
	svc, ms, _, ctx := wastedSvcEnv(t)
	pool := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()).Pool()

	// Three policies whose only difference is the monthly ceiling, so the rung
	// that won is readable straight off the verdict.
	for name, limit := range map[string]int64{
		"tiny": 1_000_000, "medium": 50_000_000, "large": 500_000_000,
	} {
		require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
			Name: name, Kind: "window_spend_cap",
			SpendWindows: []billingservice.SpendLimitWindowInput{
				{Key: "monthly", WindowSeconds: 30 * 24 * 3600, Limit: limit},
			},
		}))
	}

	payer := or897ArrearsPayer(t, ctx, ms, pool)
	// A line far above every ceiling under test, so the verdict can only ever be
	// the WINDOW talking — never affordability.
	require.NoError(t, ms.SetCreditLimit(ctx, payer, money.DefaultCurrency, 10_000_000_000))
	const tier = "gold"
	admits := func(amount int64) bool {
		in := or897Admit(payer, amount)
		in.TrustLevel = tier
		res, err := svc.Admit(ctx, in)
		require.NoError(t, err)
		return res.Allowed
	}

	// Rung 1 — merchant default.
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{PolicyName: "tiny"}))
	require.False(t, admits(2_000_000), "the merchant default applies when nothing more specific is bound")

	// Rung 2 — the tier binding beats the default.
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{PolicyName: "medium", Tier: tier}))
	require.True(t, admits(2_000_000), "the tier binding must beat the merchant default")
	require.False(t, admits(100_000_000), "...and it must be the tier's ceiling that applies")

	// Rung 3 — the payer's own binding beats both.
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{
		PolicyName: "large", CustomerID: payer.UUID().String(),
	}))
	require.True(t, admits(100_000_000), "the per-customer binding must beat the tier and the default")
}

// Rebinding is the merchant's runtime lever, so it must take effect on the NEXT
// admit — not at the end of the policy cache's TTL. A revoked policy that keeps
// admitting for fifteen minutes is a cap nobody chose.
func TestOr897_RebindingTakesEffectImmediately(t *testing.T) {
	svc, ms, _, ctx := wastedSvcEnv(t)
	pool := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()).Pool()

	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "generous", Kind: "window_spend_cap",
		SpendWindows: []billingservice.SpendLimitWindowInput{{Key: "monthly", WindowSeconds: 30 * 24 * 3600, Limit: 500_000_000}},
	}))
	require.NoError(t, svc.SetBillingPolicy(ctx, billingservice.BillingPolicyInput{
		Name: "strict", Kind: "window_spend_cap",
		SpendWindows: []billingservice.SpendLimitWindowInput{{Key: "monthly", WindowSeconds: 30 * 24 * 3600, Limit: 1_000_000}},
	}))
	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{PolicyName: "generous"}))

	payer := or897ArrearsPayer(t, ctx, ms, pool)
	require.NoError(t, ms.SetCreditLimit(ctx, payer, money.DefaultCurrency, 10_000_000_000))
	res, err := svc.Admit(ctx, or897Admit(payer, 10_000_000))
	require.NoError(t, err)
	require.True(t, res.Allowed, "warms the policy cache under the generous binding")

	require.NoError(t, svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{PolicyName: "strict"}))

	res, err = svc.Admit(ctx, or897Admit(payer, 10_000_000))
	require.NoError(t, err)
	require.False(t, res.Allowed, "the tightened binding must bite on the very next admit")
	require.Equal(t, admission.DenyBudgetExceeded, res.DenyCode)
}

// One merchant's policies and bindings must be invisible to another, under the
// same RLS-enforcing handle the production path uses.
func TestOr897_PolicyRegistryIsMerchantIsolated(t *testing.T) {
	svcA, _, _, ctxA := wastedSvcEnv(t)
	pool := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()).Pool()

	require.NoError(t, svcA.SetBillingPolicy(ctxA, billingservice.BillingPolicyInput{
		Name: "merchant_a_only", Kind: "outstanding_cap", OutstandingCapAmount: 42_000_000,
	}))
	require.NoError(t, svcA.BindBillingPolicy(ctxA, billingservice.BillingPolicyBindingInput{PolicyName: "merchant_a_only"}))

	merchantB := merchant.ID(uuid.New())
	_, err := pool.Exec(context.Background(), `
		INSERT INTO openrails.merchants (id, slug, status)
		VALUES ($1, $2, 'active')
		ON CONFLICT (slug) WHERE deleted_at IS NULL AND permission_group_id IS NULL DO UPDATE SET updated_at = now()
	`, merchantB.UUID(), "or897-policy-isolation")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.merchants WHERE id = $1", merchantB.UUID())
	})
	ctxB := merchant.WithID(context.Background(), merchantB)

	policies, err := svcA.ListBillingPolicies(ctxB)
	require.NoError(t, err)
	require.Empty(t, policies, "merchant B must not see merchant A's policies")

	bindings, err := svcA.ListBillingPolicyBindings(ctxB)
	require.NoError(t, err)
	require.Empty(t, bindings, "merchant B must not see merchant A's bindings")

	// Merchant A still sees its own.
	policies, err = svcA.ListBillingPolicies(ctxA)
	require.NoError(t, err)
	require.Len(t, policies, 1)
	require.Equal(t, "merchant_a_only", policies[0].Name)
}

// The service API and the manifest loader run the SAME normalizer, so a policy
// one accepts is a policy the other accepts, and a policy one refuses the other
// refuses with the same reason.
func TestOr897_ValidatorIsSharedByBothDeclarationPaths(t *testing.T) {
	svc, _, _, ctx := wastedSvcEnv(t)

	for _, tc := range []struct {
		name string
		in   billingservice.BillingPolicyInput
		want string
	}{
		{"no kind", billingservice.BillingPolicyInput{Name: "p"}, "kind is required"},
		{"unknown kind", billingservice.BillingPolicyInput{Name: "p", Kind: "spend_cap"}, `unknown kind "spend_cap"`},
		{"rate cap with no rate", billingservice.BillingPolicyInput{Name: "p", Kind: "accrual_rate_cap"}, "requires a positive accrual_rate_cap_per_hour"},
		{"per-policy cycle boundary", billingservice.BillingPolicyInput{
			Name: "p", Kind: "outstanding_cap", CollectionCycleBoundary: "calendar_month",
		}, "cannot be per-policy"},
		{"cross-kind limit", billingservice.BillingPolicyInput{
			Name: "p", Kind: "outstanding_cap",
			SpendWindows: []billingservice.SpendLimitWindowInput{{Key: "m", WindowSeconds: 60, Limit: 1}},
		}, "spend_windows belong to kind window_spend_cap"},
		{"window cap with no window", billingservice.BillingPolicyInput{Name: "p", Kind: "window_spend_cap"}, "at least one spend_windows entry"},
		{"bad name", billingservice.BillingPolicyInput{Name: "a policy", Kind: "outstanding_cap"}, "may use only letters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.SetBillingPolicy(ctx, tc.in)
			require.ErrorIs(t, err, billingservice.ErrInvalidBillingPolicy)
			require.ErrorContains(t, err, tc.want)

			// The manifest path reaches the identical normalizer, so the same
			// input yields the same refusal.
			_, _, verr := billingservice.ValidateBillingPolicy(tc.in)
			require.ErrorContains(t, verr, tc.want)
		})
	}

	// A binding that names both rungs cannot be ranked, so it is refused rather
	// than silently resolved one way.
	err := svc.BindBillingPolicy(ctx, billingservice.BillingPolicyBindingInput{
		PolicyName: "p", Tier: "gold", CustomerID: uuid.NewString(),
	})
	require.ErrorIs(t, err, billingservice.ErrInvalidBillingPolicy)
	require.ErrorContains(t, err, "a customer OR a tier, not both")
}

// or897ArrearsPayer seeds an unfunded ARREARS payer: every admit verdict below
// is then about the policy, never about a prepaid balance quietly covering it.
func or897ArrearsPayer(t *testing.T, ctx context.Context, ms *money.MoneyService, pool *pgxpool.Pool) identity.CustomerID {
	t.Helper()
	payer := identity.CustomerIDFromString(uuid.NewString())
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, payer.UUID().String())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.billing_policy_bindings WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.money_settings WHERE customer_id = $1", payer.UUID())
	})
	arrears := money.BillingModeArrears
	_, err := ms.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: &arrears})
	require.NoError(t, err)
	return payer
}

func or897SeedPaymentMethod(t *testing.T, ctx context.Context, pool *pgxpool.Pool, payer identity.CustomerID) uuid.UUID {
	t.Helper()
	pm := uuid.New()
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), "nmi")
	_, err := gen.New(pool).CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID:                   pm,
		MerchantID:           dbtest.TestMerchantID.UUID(),
		CustomerID:           payer.UUID(),
		Rail:                 "nmi",
		InitialTransactionID: "init_" + pm.String(),
		RailCustomerRef:      "vault_" + pm.String(),
		PspID:                pspID,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.payment_methods WHERE id = $1", pm)
	})
	return pm
}

func or897Admit(payer identity.CustomerID, amount int64) billingservice.AdmitInput {
	return billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:" + payer.UUID().String(), InvokerType: "payer",
		Currency: money.DefaultCurrency, EstimatedAmount: amount,
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
		SourceID:      uuid.NewString(), Source: "usage",
	}
}
