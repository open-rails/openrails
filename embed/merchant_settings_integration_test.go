//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/stretchr/testify/require"
)

func TestMerchantSettingsAtomicDocument(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	local, err := h.StartEmbeddedHost("USD").Runtime().Client(openrails.WithCurrency("USD"))
	require.NoError(t, err)
	remote := h.StartStandalone("USD").Client()
	clients := []*openrails.Client{local, remote}
	amount, days, email := int64(500_000), 9, "operator@example.test"
	routing := []openrails.CheckoutRoutingRule{{Prefer: []string{"nmi"}}}
	document := openrails.MerchantSettings{
		Profile:                    &openrails.MerchantProfileInput{DisplayName: "Atomic merchant", SignupURL: "https://example.test/signup"},
		AutoTopupSafety:            &openrails.AutoTopupSafetyPolicy{MaxDaily: 2, MaxWeekly: 7, MaxMonthly: 20, DeclinesBeforeDisable: 2},
		InvoiceCollectionThreshold: &amount, InvoiceMonthlyFloor: &amount, InvoiceBillingBoundary: "calendar_month",
		AlertEmail: &email, RepriceNoticeWindowDays: &days, ArrearsGraceDays: &days, ArrearsDelinquencyFloor: &amount,
		CheckoutRouting:                   &routing,
		BillingPolicies:                   []openrails.BillingPolicyInput{{Name: "gold", Kind: "accrual_rate_cap", AccrualRateCapPerHour: 10_000_000}},
		BillingPolicyBindings:             []openrails.BillingPolicyBindingInput{{PolicyName: "gold"}},
		DelegatedInvokerWastedSpendLimits: []openrails.BudgetWindowInput{{Key: "short", WindowSeconds: 300, Limit: 500_000, Currency: "USD"}},
	}
	read := func(c *openrails.Client) (openrails.MerchantSettings, string) {
		t.Helper()
		got, err := c.GetMerchantSettings(ctx)
		require.NoError(t, err)
		raw, err := json.Marshal(got)
		require.NoError(t, err)
		return *got, string(raw)
	}

	// A database failure after the config/schedule writes must roll everything back.
	_, err = h.Pool().Exec(ctx, `CREATE FUNCTION openrails.issue999_fail_binding() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.tier = 'issue999-fail' THEN RAISE EXCEPTION 'injected issue999 binding failure'; END IF; RETURN NEW; END $$;
CREATE TRIGGER issue999_fail_binding BEFORE INSERT ON openrails.billing_policy_bindings FOR EACH ROW EXECUTE FUNCTION openrails.issue999_fail_binding();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := h.Pool().Exec(context.Background(), `DROP TRIGGER IF EXISTS issue999_fail_binding ON openrails.billing_policy_bindings; DROP FUNCTION IF EXISTS openrails.issue999_fail_binding();`)
		require.NoError(t, err)
	})
	for i, client := range clients {
		t.Run([]string{"embedded", "remote"}[i], func(t *testing.T) {
			require.NoError(t, client.SetMerchantSettings(ctx, document))
			got, before := read(client)
			require.Equal(t, document.AutoTopupSafety, got.AutoTopupSafety)
			require.Equal(t, document.InvoiceCollectionThreshold, got.InvoiceCollectionThreshold)
			require.Equal(t, document.CheckoutRouting, got.CheckoutRouting)
			require.NoError(t, client.SetMerchantSettings(ctx, got))
			_, after := read(client)
			require.JSONEq(t, before, after)

			bad := got
			bad.Profile = &openrails.MerchantProfileInput{DisplayName: "must not commit"}
			bad.BillingPolicyBindings = []openrails.BillingPolicyBindingInput{{PolicyName: "missing"}}
			require.ErrorIs(t, client.SetMerchantSettings(ctx, bad), openrails.ErrInvalid)
			_, after = read(client)
			require.JSONEq(t, before, after)
			bad.BillingPolicyBindings = []openrails.BillingPolicyBindingInput{{PolicyName: "gold", Tier: "issue999-fail"}}
			require.ErrorIs(t, client.SetMerchantSettings(ctx, bad), openrails.ErrInternal)
			_, after = read(client)
			require.JSONEq(t, before, after)
		})
	}

	// A second runtime must see both tightening and loosening immediately.
	payer := openrails.CustomerID(uuid.New())
	_, err = local.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &payer, Invoker: "issue999", Currency: "USD", Amount: 20_000_000, Source: "issue999", SourceID: uuid.NewString()})
	require.NoError(t, err)
	admit := func(expected bool) {
		t.Helper()
		deadline := time.Now().Add(time.Hour).Unix()
		requestID := uuid.NewString()
		result, err := remote.Admit(ctx, openrails.AdmitRequest{CustomerID: payer.String(), Invoker: "issue999", InvokerType: "payer", Currency: "USD", Source: "issue999", RequestID: requestID, ExpiresAt: &deadline, AccrualRateDeltaPerHour: 11_000_000, EstimatedAmount: 1000})
		require.NoError(t, err)
		require.Equal(t, expected, result.Allowed)
		if result.Allowed {
			require.NoError(t, remote.Release(ctx, requestID))
		}
	}
	admit(false)
	document.BillingPolicies[0].AccrualRateCapPerHour = 12_000_000
	require.NoError(t, local.SetMerchantSettings(ctx, document))
	admit(true)
	document.BillingPolicies[0].AccrualRateCapPerHour = 10_000_000
	require.NoError(t, local.SetMerchantSettings(ctx, document))
	admit(false)

	// Runtime customer bindings survive document roundtrips and prevent removal
	// of a policy they still reference, even when its FK is configured to cascade.
	_, err = h.Pool().Exec(ctx, `INSERT INTO openrails.billing_policy_bindings(id,merchant_id,customer_id,policy_name,created_at,updated_at) VALUES($1,$2,$3,'gold',now(),now())`, uuid.New(), dbtest.TestMerchantID.UUID(), payer.UUID())
	require.NoError(t, err)
	got, before := read(remote)
	require.NoError(t, remote.SetMerchantSettings(ctx, got))
	require.ErrorIs(t, remote.SetMerchantSettings(ctx, openrails.MerchantSettings{}), openrails.ErrInvalid)
	_, after := read(remote)
	require.JSONEq(t, before, after)
}
