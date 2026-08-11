//go:build integration

package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprouter "github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/identity"
)

// TestCustomerDelegationSpend_HTTP_EndToEnd drives the WHOLE customer-delegation
// path through the REAL HTTP surfaces against the suite's live Postgres + Redis:
//
//  1. a CUSTOMER, authenticated as a delegated principal, grants a delegate (an
//     "invoker") a spend-delegation budget window via the customer-treasury route
//     PUT /v1/customers/:customer_id/spend-delegations (the only thing a customer
//     can do with its group — let another principal spend its balance, bounded by
//     a budget window enforced OpenRails-side);
//  2. the delegate's spend is admitted through the merchant admissions route
//     (POST /v1/merchant/admissions, invoker_type=delegated) — a spend WITHIN the
//     window succeeds, a spend EXCEEDING the window is denied (budget_exceeded);
//  3. a DIFFERENT delegate under the SAME customer that holds NO grant is denied
//     (delegated_spend_not_allowed) — per-delegate isolation; the grant is the
//     gate, not mere membership in the customer group.
//
// This is the HTTP-level counterpart of the module-level
// internal/modules/admission/admitter_integration_test.go: it proves the
// customer-set delegation (treasury HTTP) and the spendgate enforcement (admit
// HTTP) are wired together end-to-end on the SAME payer, which neither existing
// test covers (the treasury test only stores the policy; the admit HTTP test only
// drives a direct-payer credential).
func TestCustomerDelegationSpend_HTTP_EndToEnd(t *testing.T) {
	suite := getSharedTestSuite(t)
	require.NotNil(t, suite.App.Runtime.RedisClient, "admit needs Redis")
	ctx := suite.MerchantCtx()

	// The customer-treasury surface rebinds the payer to the customer's payable
	// subject = the customer/merchant uuid (#567), so both the delegation it stores
	// and the admit below must target the SAME payer: the test merchant uuid.
	payerID := dbtest.TestMerchantID.UUID()
	payer := identity.CustomerID(payerID)
	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", payerID)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
	})

	// Fund the customer balance well past every spend below, so the BUDGET WINDOW —
	// not affordability — is the binding gate for the delegate.
	ms := money.NewMoneyService(suite.App.Runtime.DB)
	_, err := ms.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payerID.String(), Currency: money.DefaultCurrency, Amount: 1_000_000_000, Source: "seed",
	})
	require.NoError(t, err)

	delegate := "user:delegate-" + uuid.NewString()
	stranger := "user:stranger-" + uuid.NewString()

	// --- 1) The customer grants the delegate a $1000-per-day spend window via the
	// real customer-treasury HTTP route. ---
	treasury := http.NewServeMux()
	httproutes.RegisterCustomerTreasuryRoutes(httprouter.NewMux(treasury, "/v1/customers", suite.App.Runtime), suite.App.Runtime,
		middleware.DelegatedPrincipalRequired(hostSeamAuthenticator{
			subject: uuid.NewString(),
			perms: []string{
				// or#916: the merchant-as-payer path binds only for merchant admins.
				permissions.MerchantAll,
				controlplane.PermCustomerSpendDelegationsRead,
				controlplane.PermCustomerSpendDelegationsUpdate,
			},
		}), routesurface.AllProviderRoutes())
	treasurySrv := httptest.NewServer(treasury)
	t.Cleanup(treasurySrv.Close)

	delegationDoc := map[string]any{"delegations": []map[string]any{{
		"scope":     "invoker",
		"scope_key": delegate,
		"windows": []map[string]any{
			{"key": "day", "window_seconds": 86400, "limit": 1000, "currency": money.DefaultCurrency},
		},
	}}}
	resp := requestCustomerTreasuryJSON(t, treasurySrv, http.MethodPut,
		"/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations", delegationDoc)
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	// --- Mount the real merchant admissions surface (service-credential auth). ---
	admitMux := http.NewServeMux()
	resolver := stubServiceCredentialResolver{
		permissions: []string{controlplane.PermMerchantAdmissionsCreate},
	}
	httproutes.RegisterServiceRoutes(httprouter.NewMux(admitMux, "/v1/merchant", suite.App.Runtime), suite.App.Runtime,
		httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{ServiceCredentialResolver: resolver})})
	admitSrv := httptest.NewServer(middleware.ChainHTTP(admitMux, middleware.ResolveMerchantHTTP(middleware.StaticMerchant(dbtest.TestMerchantID))))
	t.Cleanup(admitSrv.Close)

	type admitResp struct {
		Allowed   bool   `json:"allowed"`
		BlockedBy string `json:"blocked_by"`
		DenyCode  string `json:"deny_code"`
	}
	type admitVerdict struct {
		Status int       `json:"status"`
		Result admitResp `json:"result"`
	}
	admitDelegated := func(invoker, reqID string, amount int64) admitVerdict {
		body := map[string]any{"items": []map[string]any{{
			"customer_id": payerID.String(), "invoker": invoker, "invoker_type": "delegated",
			"currency": money.DefaultCurrency, "estimated_amount": amount, "request_id": reqID,
		}}}
		data, mErr := json.Marshal(body)
		require.NoError(t, mErr)
		req, rErr := http.NewRequest(http.MethodPost, admitSrv.URL+"/v1/merchant/admissions", bytes.NewReader(data))
		require.NoError(t, rErr)
		req.Header.Set("Authorization", "Bearer openrails_st_testkeyid_testsecret")
		req.Header.Set("Content-Type", "application/json")
		httpResp, dErr := admitSrv.Client().Do(req)
		require.NoError(t, dErr)
		defer httpResp.Body.Close()
		raw, _ := io.ReadAll(httpResp.Body)
		require.Equal(t, http.StatusOK, httpResp.StatusCode, string(raw)) // batch envelope is always 200
		var batch struct {
			Items []admitVerdict `json:"items"`
		}
		require.NoError(t, json.Unmarshal(raw, &batch), string(raw))
		require.Len(t, batch.Items, 1)
		return batch.Items[0]
	}

	reqPrefix := uuid.NewString()

	// --- 2a) Delegate spends 600 WITHIN the $1000 window → allowed. ---
	a1 := admitDelegated(delegate, reqPrefix+"-r1", 600)
	require.Equal(t, http.StatusOK, a1.Status, "first 600 fits the 1000 delegate window")
	require.True(t, a1.Result.Allowed)

	// --- 2b) Delegate's next 600 (1200 > 1000) EXCEEDS the window → denied. ---
	a2 := admitDelegated(delegate, reqPrefix+"-r2", 600)
	require.Equal(t, http.StatusForbidden, a2.Status, "600+600 breaches the 1000 delegate window")
	require.False(t, a2.Result.Allowed)
	require.Equal(t, "budget", a2.Result.BlockedBy)
	require.Equal(t, admission.DenyBudgetExceeded, a2.Result.DenyCode)

	// --- 3) A DIFFERENT delegate under the SAME customer with NO grant is denied:
	// membership in the customer group is not enough — a spend needs an explicit
	// spend-delegation, and that delegation is per-delegate (not a shared pool). ---
	a3 := admitDelegated(stranger, reqPrefix+"-r3", 1)
	require.Equal(t, http.StatusForbidden, a3.Status)
	require.False(t, a3.Result.Allowed)
	require.Equal(t, "budget", a3.Result.BlockedBy)
	require.Equal(t, admission.DenyDelegatedSpendNotAllowed, a3.Result.DenyCode)
}
