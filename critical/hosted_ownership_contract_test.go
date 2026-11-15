//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestHostedOwnershipContracts(t *testing.T) {
	t.Run("OR-J01-S16/host-admission-preserves-proof-and-billing-scope", hostedCustomerAdmission)
}

func hostedCustomerAdmission(t *testing.T) {
	w := newStandaloneWorld(t, func(cfg map[string]any) { cfg["CustomerAdmissionPolicy"] = true; cfg["MappedCustomers"] = true })
	var native struct {
		Value string `json:"value"`
	}
	w.command(t, map[string]string{"Op": "native-customer-token", "Merchant": "alpha", "Account": "alice"}, &native)
	require.NotEmpty(t, native.Value)
	customer := billing.CustomerID(uuid.MustParse(w.AliceID))
	status, raw := w.request(t, http.MethodGet, "/v1/me/orders", native.Value, nil)
	require.Equal(t, http.StatusForbidden, status, string(raw))
	_, err := w.alpha.GetCustomer(t.Context(), customer)
	require.ErrorIs(t, err, billing.ErrNotFound, "policy refuses before customer allocation")
	var policy map[string]bool
	w.command(t, map[string]string{"Op": "customer-policy", "ID": "allow"}, &policy)
	status, raw = w.request(t, http.MethodGet, "/v1/me/orders", native.Value, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	w.command(t, map[string]string{"Op": "customer-policy", "ID": "deny"}, &policy)
	status, raw = w.request(t, http.MethodGet, "/v1/me/orders", native.Value, nil)
	require.Equal(t, http.StatusForbidden, status, string(raw), "existing row does not bypass host policy")
	_, err = w.alpha.GetCustomer(t.Context(), customer)
	require.NoError(t, err, "staff reads keep their own permission gate")
	// Merchant-asserted identities keep ordinary merchant-specific access.
	status, raw = w.request(t, http.MethodGet, "/v1/me/orders", w.Merchants["alpha"].CustomerToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = w.request(t, http.MethodGet, "/__mapped/alpha/me/orders", w.AliceToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw), "host mapping is separate from automatic admission")
	for _, entry := range []struct{ method, path string }{{http.MethodGet, "/v1/network/links"}, {http.MethodPost, "/v1/me/network-link/challenges"}, {http.MethodGet, "/pay/sign-in"}} {
		status, raw = w.request(t, entry.method, entry.path, native.Value, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
	}
	w.command(t, map[string]string{"Op": "grant-customer-owner", "Merchant": "alpha", "Account": "alice"}, &policy)
	var token struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	w.command(t, map[string]any{"Op": "authorize", "Merchant": "alpha", "Account": "alice", "Scopes": []string{"openid", "fixture:host", billing.ScopeSelf}}, &token)
	require.Contains(t, strings.Fields(token.Scope), "fixture:host")
	w.command(t, map[string]string{"Op": "customer-policy", "ID": "allow"}, &policy)
	status, raw = w.request(t, http.MethodGet, "/v1/me/orders", token.AccessToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = w.request(t, http.MethodGet, "/v1/admin/customers", token.AccessToken, nil)
	require.Equal(t, http.StatusForbidden, status, string(raw), "host override cannot broaden canonical self scope")
	w.command(t, map[string]string{"Op": "legacy-account-fact", "Merchant": "alpha", "Account": "alice"}, &policy)
	events, err := w.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"customer.network_linked"}})
	require.NoError(t, err)
	require.Len(t, events.Events, 1)
	require.Equal(t, "customer.network_linked", events.Events[0].Type)
	var legacy struct {
		Legacy bool `json:"legacy"`
	}
	require.NoError(t, json.Unmarshal(events.Events[0].Data, &legacy))
	require.True(t, legacy.Legacy)
	status, raw = w.request(t, http.MethodGet, "/v1/config", "", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var configuration map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &configuration))
	require.NotContains(t, configuration, "network")
}
