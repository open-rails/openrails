//go:build integration

package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprouter "github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// or#897 wire contract, through the REAL merchant settings route:
//
//   - the retired `trust_level_spend_limits` key fails LOUDLY with a rename
//     error rather than being ignored — a silently-dropped spend cap is a cap
//     nobody is enforcing, and the caller would never learn;
//   - `billing_policies` + `billing_policy_bindings` install and read back;
//   - a malformed policy is a 400 from the SHARED normalizer, and nothing is
//     written (the document is validated whole before any of it lands).
func TestOr897_MerchantSettingsWire(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := suite.MerchantCtx()
	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.billing_policy_bindings")
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.billing_policies")
	})

	mux := http.NewServeMux()
	resolver := stubServiceCredentialResolver{permissions: []string{
		controlplane.PermMerchantSettingsRead, controlplane.PermMerchantSettingsUpdate,
	}}
	httproutes.RegisterServiceRoutes(httprouter.NewMux(mux, "/v1/merchant", suite.App.Runtime), suite.App.Runtime,
		httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{ServiceCredentialResolver: resolver})})
	router := middleware.ChainHTTP(mux, middleware.ResolveMerchantHTTP(middleware.StaticMerchant(dbtest.TestMerchantID)))

	call := func(method, path string, body any) *httptest.ResponseRecorder {
		var rdr *bytes.Reader
		if body != nil {
			b, err := json.Marshal(body)
			require.NoError(t, err)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader([]byte("{}"))
		}
		req := httptest.NewRequest(method, path, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer openrails_st_testkeyid_testsecret")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// The retired key: refused, and the refusal names its replacement.
	res := call(http.MethodPut, "/v1/merchant/settings", map[string]any{
		"trust_level_spend_limits": []map[string]any{{
			"trust_level":    "gold",
			"budget_windows": []map[string]any{{"key": "hourly", "window_seconds": 3600, "limit": 10_000}},
		}},
	})
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
	require.Contains(t, res.Body.String(), "billing_policies")
	require.Contains(t, res.Body.String(), "billing_policy_bindings")

	// A malformed policy is a client error from the shared normalizer.
	res = call(http.MethodPut, "/v1/merchant/settings", map[string]any{
		"billing_policies": []map[string]any{{"name": "quota", "kind": "accrual_rate_cap"}},
	})
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
	require.Contains(t, res.Body.String(), "not implemented yet")

	// Nothing from the refused document was written.
	var count int
	require.NoError(t, suite.Pool.QueryRow(ctx, "SELECT count(*) FROM openrails.billing_policies").Scan(&count))
	require.Zero(t, count, "a refused settings document must leave no partial policy behind")

	// The replacement keys install, and the GET reads them back.
	res = call(http.MethodPut, "/v1/merchant/settings", map[string]any{
		"billing_policies": []map[string]any{
			{"name": "api_line", "kind": "outstanding_cap", "outstanding_cap_amount": 200_000_000},
			{"name": "cloud_monthly", "kind": "window_spend_cap", "spend_windows": []map[string]any{
				{"key": "monthly", "window_seconds": 30 * 24 * 3600, "limit": 2_000_000_000},
			}},
		},
		"billing_policy_bindings": []map[string]any{
			{"policy": "api_line"},
			{"policy": "cloud_monthly", "tier": "cloud"},
		},
	})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	res = call(http.MethodGet, "/v1/merchant/settings", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	var got struct {
		BillingPolicies []struct {
			Name                 string `json:"name"`
			Kind                 string `json:"kind"`
			OutstandingCapAmount int64  `json:"outstanding_cap_amount"`
			SpendWindows         []struct {
				Key   string `json:"key"`
				Limit int64  `json:"limit"`
			} `json:"spend_windows"`
		} `json:"billing_policies"`
		BillingPolicyBindings []struct {
			Policy string `json:"policy"`
			Tier   string `json:"tier"`
		} `json:"billing_policy_bindings"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &got))
	require.Len(t, got.BillingPolicies, 2)
	require.Equal(t, "api_line", got.BillingPolicies[0].Name)
	require.Equal(t, "outstanding_cap", got.BillingPolicies[0].Kind)
	require.EqualValues(t, 200_000_000, got.BillingPolicies[0].OutstandingCapAmount)
	require.Equal(t, "cloud_monthly", got.BillingPolicies[1].Name)
	require.Len(t, got.BillingPolicies[1].SpendWindows, 1)
	require.EqualValues(t, 2_000_000_000, got.BillingPolicies[1].SpendWindows[0].Limit)

	require.Len(t, got.BillingPolicyBindings, 2)
	require.Equal(t, "cloud", got.BillingPolicyBindings[0].Tier, "the tier rung sorts before the default")
	require.Equal(t, "cloud_monthly", got.BillingPolicyBindings[0].Policy)
	require.Empty(t, got.BillingPolicyBindings[1].Tier)
	require.Equal(t, "api_line", got.BillingPolicyBindings[1].Policy)
}
