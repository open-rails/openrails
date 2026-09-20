//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestPolicyDelegationAndAdmissionWorkflow(t *testing.T) {
	f := newTreasuryWorkflow(t)
	t.Run("delegation_document", func(t *testing.T) { checkPolicyDelegationDocument(t, f) })
	t.Run("settings_and_precedence", func(t *testing.T) { checkPolicyPrecedence(t, f) })
	t.Run("budget_effects", func(t *testing.T) { checkPolicyBudgetEffects(t, f) })
	t.Run("waste_and_profiles", func(t *testing.T) { checkPolicyWasteAndProfiles(t, f) })
	t.Run("invoker_windows", func(t *testing.T) { checkPolicyInvokerWindows(t, f) })
}

func checkPolicyDelegationDocument(t *testing.T, f treasuryWorkflow) {
	ctx := t.Context()
	payer, token := f.actor(t, []string{controlplane.PermCustomerSpendDelegationsRead, controlplane.PermCustomerSpendDelegationsUpdate})
	caller := f.surface.RegisterServiceJWTIssuer("policy-service-"+uuid.NewString()[:8], f.merchant.MerchantSlug,
		[]string{controlplane.PermMerchantCustomerSettingsUpdate})
	machine, err := openrails.NewRemote(f.surface.BaseURL, openrails.WithCurrency("USD"), openrails.WithMerchantID(f.merchant.MerchantID),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return caller.Token, nil }))
	require.NoError(t, err)
	path := f.hostURL + "/v1/customers/" + payer.String() + "/spend-delegations"
	read := func() []openrails.SpendDelegationInput {
		t.Helper()
		status, raw := requestWorkflowJSON(t, http.MethodGet, path, token, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var doc struct {
			Delegations []openrails.SpendDelegationInput `json:"delegations"`
		}
		require.NoError(t, json.Unmarshal(raw, &doc))
		return doc.Delegations
	}
	invoker := openrails.SpendDelegationInput{Scope: "invoker", ScopeKey: "worker-" + uuid.NewString(), Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 1200, Currency: "USD"}}}
	role := openrails.SpendDelegationInput{Scope: "role", ScopeKey: uuid.NewString(), Windows: []openrails.SpendLimitWindow{{Key: "week", WindowSeconds: 604800, Limit: 9000, Currency: "USD"}}}
	tier := openrails.SpendDelegationInput{Scope: "invoker_tier", ScopeKey: "trust_1", Windows: []openrails.SpendLimitWindow{{Key: "month", WindowSeconds: 2592000, Limit: 15000, Currency: "USD"}}}
	status, raw := requestWorkflowJSON(t, http.MethodPut, path, token, map[string]any{"delegations": []openrails.SpendDelegationInput{invoker, role, tier}})
	require.Equal(t, http.StatusOK, status, string(raw))
	invoker.Windows[0].Limit = 321
	require.NoError(t, machine.SetCustomerSpendDelegation(ctx, payer, invoker))
	require.ElementsMatch(t, []openrails.SpendDelegationInput{invoker, role, tier}, read(), "singular upsert preserves sibling role and tier grants")
	invoker.Windows[0].Limit = 322
	status, raw = requestWorkflowJSON(t, http.MethodPut, path+":upsert", token, invoker)
	require.Equal(t, http.StatusOK, status, string(raw))
	require.ElementsMatch(t, []openrails.SpendDelegationInput{invoker, role, tier}, read(), "customer upsert also preserves sibling grants")
	role.Provenance = "sha256:" + strings.Repeat("ab", 32)
	role.Windows = []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 500, Currency: "USD"}}
	status, raw = requestWorkflowJSON(t, http.MethodPut, path, token, map[string]any{"delegations": []openrails.SpendDelegationInput{role}})
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Equal(t, []openrails.SpendDelegationInput{role}, read(), "customer replacement removes omitted invoker and tier grants")
	require.NoError(t, machine.SetCustomerSpendDelegation(ctx, payer, invoker))
	require.NoError(t, machine.SetCustomerSpendDelegations(ctx, payer, []openrails.SpendDelegationInput{role}))
	require.Equal(t, []openrails.SpendDelegationInput{role}, read())
	duplicate := role
	duplicate.Scope, duplicate.ScopeKey = " role ", " "+role.ScopeKey+" "
	err = machine.SetCustomerSpendDelegations(ctx, payer, []openrails.SpendDelegationInput{role, duplicate})
	require.ErrorIs(t, err, openrails.ErrInvalid)
	var refusal *openrails.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusBadRequest, refusal.Status)
	require.Contains(t, err.Error(), "duplicate delegation for role")
	require.Equal(t, []openrails.SpendDelegationInput{role}, read(), "refused replacement changes nothing")
	status, raw = requestWorkflowJSON(t, http.MethodPut, path, token, map[string]any{"customer_id": uuid.NewString(), "delegations": []any{}})
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	require.NoError(t, machine.SetCustomerSpendDelegation(ctx, payer, invoker))
	require.ElementsMatch(t, []openrails.SpendDelegationInput{invoker, role}, read(), "provenance belongs only to its own grant")
	status, raw = requestWorkflowJSON(t, http.MethodDelete, path+"/invoker/"+invoker.ScopeKey, token, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Equal(t, true, workflowObject(t, raw)["deleted"])
	require.Equal(t, []openrails.SpendDelegationInput{role}, read())
	status, raw = requestWorkflowJSON(t, http.MethodDelete, path+"/invoker/"+invoker.ScopeKey, token, nil)
	require.Equal(t, http.StatusNotFound, status, string(raw))
	require.Contains(t, string(raw), "spend_delegation_not_found")
	status, raw = requestWorkflowJSON(t, http.MethodDelete, path+"/bogus/"+invoker.ScopeKey, token, nil)
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	require.NoError(t, machine.DeleteCustomerSpendDelegation(ctx, payer, "role", role.ScopeKey))
	require.ErrorIs(t, machine.DeleteCustomerSpendDelegation(ctx, payer, "role", role.ScopeKey), openrails.ErrNotFound)
	require.Empty(t, read())
}

func checkPolicyPrecedence(t *testing.T, f treasuryWorkflow) {
	ctx := t.Context()
	settingsURL := f.surface.BaseURL + "/v1/merchant/settings"
	for _, invalid := range []map[string]any{
		{"trust_level_spend_limits": []any{}},
		{"billing_policies": []any{map[string]any{"name": "quota", "kind": "accrual_rate_cap"}}},
	} {
		status, raw := requestWorkflowJSON(t, http.MethodPut, settingsURL, f.merchant.APIKey, invalid)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
	}
	for _, invalid := range []openrails.BillingPolicyInput{
		{Name: "p"}, {Name: "p", Kind: "spend_cap"}, {Name: "p", Kind: "accrual_rate_cap"},
		{Name: "p", Kind: "outstanding_cap", CollectionCycleBoundary: "calendar_month"},
		{Name: "p", Kind: "outstanding_cap", SpendWindows: []openrails.BudgetWindowInput{{Key: "m", WindowSeconds: 60, Limit: 1}}},
		{Name: "p", Kind: "window_spend_cap"}, {Name: "a policy", Kind: "outstanding_cap"},
	} {
		require.ErrorIs(t, f.client.SetMerchantSettings(ctx, openrails.MerchantSettings{BillingPolicies: []openrails.BillingPolicyInput{invalid}}), openrails.ErrInvalid)
	}
	before, err := f.client.GetMerchantSettings(ctx)
	require.NoError(t, err)
	require.Empty(t, before.BillingPolicies, "invalid documents must not install partial policies")
	threshold, grace := int64(50_000_000), 7
	doc := openrails.MerchantSettings{BillingPolicies: []openrails.BillingPolicyInput{
		{Name: "api_line", Kind: "outstanding_cap", OutstandingCapAmount: 200_000_000},
		{Name: "cloud_monthly", Kind: "window_spend_cap", SpendWindows: []openrails.BudgetWindowInput{{Key: "monthly", WindowSeconds: 30 * 24 * 3600, Limit: 2_000_000_000}}},
		{Name: "cloud_quota", Kind: "accrual_rate_cap", AccrualRateCapPerHour: 10_000_000, AccrualRateWindowSeconds: 900, CollectionThresholdAmount: &threshold, DelinquencyGraceDays: &grace},
	}, BillingPolicyBindings: []openrails.BillingPolicyBindingInput{{PolicyName: "api_line"}, {PolicyName: "cloud_monthly", Tier: "cloud"}}}
	require.NoError(t, f.client.SetMerchantSettings(ctx, doc))
	stored, err := f.client.GetMerchantSettings(ctx)
	require.NoError(t, err)
	require.Len(t, stored.BillingPolicies, 3)
	for i, expected := range doc.BillingPolicies {
		require.Equal(t, expected.Name, stored.BillingPolicies[i].Name)
		require.Equal(t, expected.Kind, stored.BillingPolicies[i].Kind)
	}
	require.EqualValues(t, 200_000_000, stored.BillingPolicies[0].OutstandingCapAmount)
	require.EqualValues(t, 2_000_000_000, stored.BillingPolicies[1].SpendWindows[0].Limit)
	require.EqualValues(t, 10_000_000, stored.BillingPolicies[2].AccrualRateCapPerHour)
	require.EqualValues(t, 900, stored.BillingPolicies[2].AccrualRateWindowSeconds)
	require.Equal(t, &threshold, stored.BillingPolicies[2].CollectionThresholdAmount)
	require.Equal(t, &grace, stored.BillingPolicies[2].DelinquencyGraceDays)
	require.Equal(t, []openrails.BillingPolicyBindingInput{{PolicyName: "cloud_monthly", Tier: "cloud"}, {PolicyName: "api_line"}}, stored.BillingPolicyBindings)

	payer, _ := f.actor(t, nil)
	require.NoError(t, f.client.SetCreditLimit(ctx, payer, "USD", 10_000_000_000))
	doc = openrails.MerchantSettings{BillingPolicies: []openrails.BillingPolicyInput{
		{Name: "tiny", Kind: "window_spend_cap", SpendWindows: []openrails.BudgetWindowInput{{Key: "month", WindowSeconds: 30 * 24 * 3600, Limit: 1_000_000}}},
		{Name: "medium", Kind: "window_spend_cap", SpendWindows: []openrails.BudgetWindowInput{{Key: "month", WindowSeconds: 30 * 24 * 3600, Limit: 50_000_000}}},
		{Name: "large", Kind: "window_spend_cap", SpendWindows: []openrails.BudgetWindowInput{{Key: "month", WindowSeconds: 30 * 24 * 3600, Limit: 500_000_000}}},
	}, BillingPolicyBindings: []openrails.BillingPolicyBindingInput{{PolicyName: "tiny"}}}
	allows := func(amount int64) bool {
		t.Helper()
		deadline := time.Now().Add(time.Hour)
		key := uuid.NewString()
		result, err := f.client.Admit(ctx, openrails.AdmitRequest{CustomerID: payer, Invoker: payer.String(), InvokerType: openrails.InvokerTypePayer, TrustLevel: "gold", Currency: "USD", EstimatedAmount: amount, RequestID: key, Source: "policy", ExpiresAt: &deadline})
		require.NoError(t, err)
		if result.Allowed {
			require.NoError(t, f.client.Release(ctx, key))
		}
		return result.Allowed
	}
	require.NoError(t, f.client.SetMerchantSettings(ctx, doc))
	require.False(t, allows(2_000_000))
	doc.BillingPolicyBindings = append(doc.BillingPolicyBindings, openrails.BillingPolicyBindingInput{PolicyName: "medium", Tier: "gold"})
	require.NoError(t, f.client.SetMerchantSettings(ctx, doc))
	require.True(t, allows(2_000_000))
	require.False(t, allows(100_000_000))
	doc.BillingPolicyBindings = append(doc.BillingPolicyBindings, openrails.BillingPolicyBindingInput{PolicyName: "large", CustomerID: payer})
	require.ErrorIs(t, f.client.SetMerchantSettings(ctx, doc), openrails.ErrInvalid, "customer bindings are not accepted in a merchant settings document")
	require.False(t, allows(100_000_000), "the refused document cannot change the current binding")
	// Customer binding currently has no public Client/HTTP operation. Keep its
	// precedence proof explicit at the existing private fixture boundary.
	svc, err := service.New(f.surface.App().Runtime)
	require.NoError(t, err)
	mctx := merchant.WithID(ctx, f.merchant.MerchantID)
	require.NoError(t, svc.BindBillingPolicy(mctx, openrails.BillingPolicyBindingInput{PolicyName: "large", CustomerID: payer}))
	require.True(t, allows(100_000_000))
	require.NoError(t, svc.BindBillingPolicy(mctx, openrails.BillingPolicyBindingInput{PolicyName: "tiny", CustomerID: payer}))
	require.False(t, allows(2_000_000), "rebinding changes the next decision, not a future cache expiry")
}
