//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
)

// #850: the merchant-admin api_host surface — GET/PUT /v1/merchant/api-host.
// The write is gated on merchant:settings:update (owner-only in the fixed #567
// catalog), validates the host format, 409s on the #734 global-uniqueness
// collision, and takes effect live: the very next request against the new Host
// resolves the merchant on this SAME running server.
func TestMerchantAPIHostRouteHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	m := surface.ProvisionOwnedMerchant("api-host-m-" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	owner := m.APIKey // MintAPIKey with nil perms -> owner role
	viewer := surface.MintAPIKey(m.MerchantSlug, "api-host-viewer", []string{controlplane.PermMerchantSettingsRead})

	url := surface.BaseURL + "/v1/merchant/api-host"
	host := "api.host-route-" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".test"

	// Unset reads as null.
	status, body := requestJSON(t, http.MethodGet, url, owner, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var got struct {
		APIHost *string `json:"api_host"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	require.Nil(t, got.APIHost)

	// Viewer (merchant:settings:read only) can read but not write.
	status, body = requestJSON(t, http.MethodGet, url, viewer, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = requestJSON(t, http.MethodPut, url, viewer, map[string]any{"api_host": host})
	require.Equal(t, http.StatusForbidden, status, string(body))

	// Format wall: scheme'd / pathed / garbage hosts are 400, never stored.
	for _, bad := range []string{"https://" + host, host + "/v1", "no spaces allowed", "under_score.test"} {
		status, body = requestJSON(t, http.MethodPut, url, owner, map[string]any{"api_host": bad})
		require.Equalf(t, http.StatusBadRequest, status, "host %q: %s", bad, string(body))
	}

	// Owner assigns (normalization strips case + numeric port).
	status, body = requestJSON(t, http.MethodPut, url, owner, map[string]any{"api_host": strings.ToUpper(host) + ":8443"})
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &got))
	require.NotNil(t, got.APIHost)
	require.Equal(t, host, *got.APIHost)

	status, body = requestJSON(t, http.MethodGet, url, owner, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &got))
	require.NotNil(t, got.APIHost)
	require.Equal(t, host, *got.APIHost)

	// Live effect (#734): a service JWT for ANOTHER merchant presented against
	// the newly-assigned Host fails closed on this same running server — proof
	// the route's write resolves immediately, no restart (mirrors
	// TestHostMerchantRegisteredAfterBootResolvesHTTP's mechanism).
	other := surface.ProvisionOwnedMerchant("api-host-other-" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	tokenOther := surface.RegisterServiceJWTIssuer(
		"api-host-issuer-"+strings.ReplaceAll(uuid.NewString(), "-", ""),
		other.MerchantSlug,
		[]string{controlplane.PermMerchantCustomerSettingsRead, controlplane.PermMerchantCustomerSettingsUpdate},
	)
	payer := uuid.NewString()
	status, body = requestJSONHost(t, http.MethodGet,
		surface.BaseURL+"/v1/merchant/credits/balance?currency=usd&customer_id="+payer, host, tokenOther.Token, nil)
	require.Containsf(t, []int{http.StatusUnauthorized, http.StatusForbidden}, status,
		"other merchant's credential on the freshly-assigned host must fail closed: %s", string(body))

	// Global uniqueness: the other merchant claiming the same host 409s.
	status, body = requestJSON(t, http.MethodPut, url, other.APIKey, map[string]any{"api_host": host})
	require.Equal(t, http.StatusConflict, status, string(body))

	// Owner clears with "" -> reads as null again.
	status, body = requestJSON(t, http.MethodPut, url, owner, map[string]any{"api_host": ""})
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &got))
	require.Nil(t, got.APIHost)

	status, body = requestJSON(t, http.MethodGet, url, owner, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &got))
	require.Nil(t, got.APIHost)

	// The freed host is claimable by the other merchant now.
	status, body = requestJSON(t, http.MethodPut, url, other.APIKey, map[string]any{"api_host": host})
	require.Equal(t, http.StatusOK, status, string(body))
}
