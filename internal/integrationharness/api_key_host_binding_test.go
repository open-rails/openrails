//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyMerchantMustMatchRequestHost(t *testing.T) {
	h := New(t, context.Background())
	surface := h.StartStandalone("usd")
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	tokenA := surface.MintAPIKey(dbtest.TestMerchantSlug, "host-key-a-"+suffix, []string{controlplane.PermMerchantCustomerSettingsRead})
	b := surface.ProvisionOwnedMerchant("host-key-b-" + suffix)
	hostA, hostB := "api.a-"+suffix+".test", "api.b-"+suffix+".test"
	surface.SetMerchantHostConfig(dbtest.TestMerchantID, hostA)
	surface.SetMerchantHostConfig(b.MerchantID, hostB)
	endpoint := surface.BaseURL + "/v1/merchant/credits/balance?currency=usd&customer_id=" + uuid.NewString()
	for _, tc := range []struct {
		name, host, token string
		wantStatus        int
	}{
		{"matching A", hostA, tokenA, http.StatusOK},
		{"matching B", hostB, b.APIKey, http.StatusOK},
		{"A key on B host", hostB, tokenA, http.StatusForbidden},
		{"B key on A host", hostA, b.APIKey, http.StatusForbidden},
		{"no host pin", "", b.APIKey, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := requestJSONHost(t, http.MethodGet, endpoint, tc.host, tc.token, nil)
			require.Equal(t, tc.wantStatus, status, string(body))
			if tc.wantStatus == http.StatusForbidden {
				require.Contains(t, string(body), "host_merchant_mismatch")
			}
		})
	}
}
