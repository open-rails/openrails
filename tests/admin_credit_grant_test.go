//go:build integration

package tests

import (
	"bytes"
	"encoding/json"
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
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

// or#906 behavioural contract, end to end through the REAL routes: the
// human-admin credit grant is once-only at the caller's key, refuses a
// changed-amount replay with 409 idempotency_key_reused, and the key-qualified
// GET answers what the key did.

func adminCreditPost(t *testing.T, h http.Handler, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req, _ := http.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAdminCreditGrant_OnceOnlyAndChangedAmountConflict(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "c9061111-1111-4111-8111-111111111111",
		[]string{controlplane.PermMerchantCreditsGrant})

	customerID := uuid.NewString()
	sourceID := "or906-admin-" + uuid.NewString()
	path := "/v1/merchant/customers/" + customerID + "/credits"
	body := map[string]any{
		"currency": money.DefaultCurrency, "amount": 5_000, "source_id": sourceID,
		"description": "or906 goodwill",
	}

	// First grant applies.
	w := adminCreditPost(t, admin, path, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var first struct {
		ID       uuid.UUID
		Amount   int64
		Replayed bool
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &first))
	require.Equal(t, int64(5_000), first.Amount)
	require.False(t, first.Replayed)

	// The identical retry is a replay, not a second credit.
	w = adminCreditPost(t, admin, path, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var again struct {
		ID       uuid.UUID
		Amount   int64
		Replayed bool
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &again))
	require.True(t, again.Replayed)
	require.Equal(t, first.ID, again.ID)

	// A changed-amount retry at the same key is a 409 caller bug.
	body["amount"] = 10_000
	w = adminCreditPost(t, admin, path, body)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "idempotency_key_reused")

	// Exactly one deposit moved money.
	payer := identity.CustomerIDFromString(customerID)
	bal, err := suite.App.Runtime.MoneyService.GetBalanceForCustomer(suite.MerchantCtx(), payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(5_000), bal.Balance)

	// Key-qualified lookup on the machine surface: what did this key do.
	mux := http.NewServeMux()
	resolver := stubServiceCredentialResolver{
		permissions: []string{controlplane.PermMerchantCustomerSettingsRead},
	}
	httproutes.RegisterServiceRoutes(httprouter.NewMux(mux, "/v1/merchant", suite.App.Runtime), suite.App.Runtime,
		httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{ServiceCredentialResolver: resolver})})
	machine := middleware.ChainHTTP(mux, middleware.ResolveMerchantHTTP(middleware.StaticMerchant(dbtest.TestMerchantID)))

	get := func(customer, source string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet,
			"/v1/merchant/credits/deposit?customer_id="+customer+"&source_id="+source, nil)
		req.Header.Set("Authorization", "Bearer openrails_st_testkeyid_testsecret")
		rec := httptest.NewRecorder()
		machine.ServeHTTP(rec, req)
		return rec
	}

	lw := get(customerID, sourceID)
	require.Equal(t, http.StatusOK, lw.Code, lw.Body.String())
	var looked struct {
		ID       uuid.UUID
		Amount   int64
		Replayed bool
	}
	require.NoError(t, json.Unmarshal(lw.Body.Bytes(), &looked))
	require.Equal(t, first.ID, looked.ID)
	require.Equal(t, int64(5_000), looked.Amount)
	require.True(t, looked.Replayed, "the movement landed earlier — LED-15 vocabulary")

	lw = get(customerID, "or906-never-"+uuid.NewString())
	require.Equal(t, http.StatusNotFound, lw.Code, lw.Body.String())
	require.Contains(t, lw.Body.String(), "deposit_not_found")
}

// The permission split is the or#906 decision: money-in does NOT ride
// merchant:customer-settings:update (which the fixed support role holds and
// which authorizes the SIBLING entitlement/product-access grants) — it needs
// merchant:credits:grant.
func TestAdminCreditGrant_RequiresCreditsGrantPermission(t *testing.T) {
	suite := getSharedTestSuite(t)
	support := newHostSeamAdminRouter(t, suite, "c9062222-2222-4222-8222-222222222222",
		[]string{controlplane.PermMerchantCustomerSettingsUpdate})

	body := map[string]any{
		"currency": money.DefaultCurrency, "amount": 5_000,
		"source_id": "or906-denied-" + uuid.NewString(),
	}
	w := adminCreditPost(t, support, "/v1/merchant/customers/"+uuid.NewString()+"/credits", body)
	require.Equal(t, http.StatusForbidden, w.Code,
		"customer-settings:update must NOT mint balance: %s", w.Body.String())
}
