//go:build integration

package tests

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/permissions"
)

// The precedence journey calls this with the same real merchant and mounted
// authority chain; none of these refusals use a test-created trusted context.
func checkCustomerPolicyBoundary(t *testing.T, f treasuryWorkflow) {
	ctx := t.Context()
	payer, _ := f.actor(t, nil)
	large := "large" // Declared by the preceding settings/precedence journey.
	_, err := f.client.SetCustomerBillingPolicy(ctx, payer, &large)
	require.NoError(t, err)
	path := f.surface.BaseURL + "/v1/merchant/customers/" + payer.String() + "/billing-policy"

	for _, invalid := range []any{
		map[string]any{}, map[string]any{"policy_name": ""}, map[string]any{"policy_name": " "},
		map[string]any{"policy_name": 1}, map[string]any{"policy_name": false}, map[string]any{"policy_name": []any{}},
		map[string]any{"policy_name": "large", "customer_id": payer},
		[]any{},
	} {
		status, raw := requestWorkflowJSON(t, http.MethodPut, path, f.merchant.APIKey, invalid)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), "invalid_param")
	}
	for _, row := range []struct {
		permission string
		get, put   int
	}{
		{controlplane.PermMerchantCustomerSettingsRead, http.StatusOK, http.StatusForbidden},
		{controlplane.PermMerchantCustomerSettingsUpdate, http.StatusForbidden, http.StatusOK},
		{controlplane.PermMerchantSettingsUpdate, http.StatusForbidden, http.StatusForbidden},
	} {
		caller := f.surface.RegisterServiceJWTIssuer("policy-access-"+uuid.NewString()[:8], f.merchant.MerchantSlug, []string{row.permission})
		status, raw := requestWorkflowJSON(t, http.MethodGet, path, caller.Token, nil)
		require.Equal(t, row.get, status, string(raw))
		status, raw = requestWorkflowJSON(t, http.MethodPut, path, caller.Token, map[string]any{"policy_name": large})
		require.Equal(t, row.put, status, string(raw))
	}
	for _, token := range []string{"", f.issuer.Mint(payer.String(), "", "", []string{permissions.CustomerAll})} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			status, raw := requestWorkflowJSON(t, method, path, token, map[string]any{"policy_name": nil})
			require.Equal(t, http.StatusUnauthorized, status, string(raw), "customer-only tokens are not merchant credentials")
		}
	}
	// A policy and customer that exist only in B look missing through A, and
	// B cannot alter A's customer even with its own unrestricted merchant key.
	other := f.surface.ProvisionOwnedMerchant("policy-other-" + uuid.NewString()[:8])
	otherClient := f.surface.Client(openrails.WithAPIKey(other.APIKey), openrails.WithMerchantID(other.MerchantID))
	foreignCustomer := openrails.CustomerID(uuid.New())
	_, err = otherClient.EnsureCustomer(ctx, foreignCustomer)
	require.NoError(t, err)
	foreignPolicy := "foreign_only"
	require.NoError(t, otherClient.SetMerchantSettings(ctx, openrails.MerchantSettings{BillingPolicies: []openrails.BillingPolicyInput{{Name: foreignPolicy, Kind: "outstanding_cap", OutstandingCapAmount: 1}}}))
	for _, client := range []*openrails.Client{f.client, f.embedded} {
		for _, id := range []openrails.CustomerID{openrails.CustomerID(uuid.New()), foreignCustomer} {
			_, err := client.GetCustomerBillingPolicy(ctx, id)
			requirePolicyNotFound(t, err, "customer_not_found")
			_, err = client.SetCustomerBillingPolicy(ctx, id, &large)
			requirePolicyNotFound(t, err, "customer_not_found")
			_, err = client.SetCustomerBillingPolicy(ctx, id, nil)
			requirePolicyNotFound(t, err, "customer_not_found")
			_, err = client.GetCustomerBillingPolicy(ctx, id)
			requirePolicyNotFound(t, err, "customer_not_found") // no implicit creation
		}
		for _, name := range []string{"missing_policy", foreignPolicy} {
			_, err := client.SetCustomerBillingPolicy(ctx, payer, &name)
			requirePolicyNotFound(t, err, "billing_policy_not_found")
		}
	}
	_, err = otherClient.SetCustomerBillingPolicy(ctx, payer, &foreignPolicy)
	requirePolicyNotFound(t, err, "customer_not_found")
	assignment, err := f.client.GetCustomerBillingPolicy(ctx, payer)
	require.NoError(t, err)
	require.Equal(t, &openrails.CustomerBillingPolicyAssignment{CustomerID: payer, PolicyName: &large}, assignment, "every refused write preserves the assignment")
	untouched, err := otherClient.GetCustomerBillingPolicy(ctx, foreignCustomer)
	require.NoError(t, err)
	require.Nil(t, untouched.PolicyName)
	for range 2 {
		status, raw := requestWorkflowJSON(t, http.MethodPut, path, f.merchant.APIKey, map[string]any{"policy_name": nil})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Equal(t, map[string]any{"customer_id": payer.String(), "policy_name": nil}, workflowObject(t, raw))
	}
	assignment, err = f.embedded.GetCustomerBillingPolicy(ctx, payer)
	require.NoError(t, err)
	require.Nil(t, assignment.PolicyName, "explicit null is visible in the other deployment")
	// Assignment and declaration replacement share the same merchant lock.
	// Either order is valid; an orphan binding or a database-error 500 is not.
	doc, err := f.client.GetMerchantSettings(ctx)
	require.NoError(t, err)
	withoutLarge := *doc
	withoutLarge.BillingPolicies = append([]openrails.BillingPolicyInput(nil), doc.BillingPolicies...)
	for i, policy := range withoutLarge.BillingPolicies {
		if policy.Name == large {
			withoutLarge.BillingPolicies = append(withoutLarge.BillingPolicies[:i], withoutLarge.BillingPolicies[i+1:]...)
			break
		}
	}
	for range 6 {
		require.NoError(t, f.client.SetMerchantSettings(ctx, *doc))
		start := make(chan struct{})
		setDone, removeDone := make(chan error, 1), make(chan error, 1)
		go func() { <-start; _, err := f.client.SetCustomerBillingPolicy(ctx, payer, &large); setDone <- err }()
		go func() { <-start; removeDone <- f.embedded.SetMerchantSettings(ctx, withoutLarge) }()
		close(start)
		setErr, removeErr := <-setDone, <-removeDone
		current, err := f.embedded.GetCustomerBillingPolicy(ctx, payer)
		require.NoError(t, err)
		if setErr == nil {
			require.ErrorIs(t, removeErr, openrails.ErrInvalid)
			require.Equal(t, &large, current.PolicyName)
		} else {
			requirePolicyNotFound(t, setErr, "billing_policy_not_found")
			require.NoError(t, removeErr)
			require.Nil(t, current.PolicyName)
		}
		_, err = f.client.SetCustomerBillingPolicy(ctx, payer, nil)
		require.NoError(t, err)
	}
}

func requirePolicyNotFound(t *testing.T, err error, code string) {
	t.Helper()
	require.ErrorIs(t, err, openrails.ErrNotFound)
	var refusal *openrails.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusNotFound, refusal.Status)
	require.Equal(t, code, refusal.Code)
}
