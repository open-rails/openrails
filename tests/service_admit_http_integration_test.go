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

// TestServiceAdmit_HTTP_EndToEnd drives the #513 spendgate admission path through
// the REAL HTTP merchant routes (POST /v1/merchant/admissions +
// /v1/merchant/admissions/:id/capture|release) against the suite's live Postgres + Redis — the full
// route → handler → Service.Admit → spendgate stack, not the in-process layer.
func TestServiceAdmit_HTTP_EndToEnd(t *testing.T) {
	suite := getSharedTestSuite(t)
	require.NotNil(t, suite.App.Runtime.RedisClient, "admit needs Redis")
	ctx := suite.MerchantCtx()

	userID := uuid.NewString()
	requestPrefix := uuid.NewString()
	payerID := suite.ensureCustomer(ctx, userID)
	payer := identity.CustomerID(payerID)
	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerID)
	})

	// Seed a $1000 balance via the post-#512 ledger deposit (no money_blocks).
	ms := money.NewMoneyService(suite.App.Runtime.DB)
	_, err := ms.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payerID.String(), Currency: money.DefaultCurrency, Amount: 1000, Source: "seed",
	})
	require.NoError(t, err)

	// Mount the same service surface the public server registers.
	mux := http.NewServeMux()
	resolver := stubServiceCredentialResolver{
		permissions: []string{controlplane.PermMerchantCustomerSettingsRead, controlplane.PermMerchantCustomerSettingsUpdate, controlplane.PermMerchantAdmissionsCreate},
	}
	httproutes.RegisterServiceRoutes(httprouter.NewMux(mux, "/v1/merchant", suite.App.Runtime), suite.App.Runtime, httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{ServiceCredentialResolver: resolver})})
	router := middleware.ChainHTTP(mux, middleware.ResolveMerchantHTTP(middleware.StaticMerchant(dbtest.TestMerchantID)))

	post := func(path string, body any) *httptest.ResponseRecorder {
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader([]byte("{}"))
		}
		req := httptest.NewRequest(http.MethodPost, path, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer openrails_st_testkeyid_testsecret")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	admitBody := func(reqID string, amount int64) map[string]any {
		return map[string]any{
			"customer_id": payerID.String(), "invoker": "user:a", "invoker_type": "payer",
			"currency": money.DefaultCurrency, "estimated_amount": amount, "request_id": reqID,
		}
	}
	type admitResp struct {
		Allowed   bool   `json:"allowed"`
		BlockedBy string `json:"blocked_by"`
		DenyCode  string `json:"deny_code"`
	}
	type admitVerdict struct {
		Status int       `json:"status"`
		Result admitResp `json:"result"`
	}
	admit := func(reqID string, amount int64) admitVerdict {
		w := post("/v1/merchant/admissions", map[string]any{"items": []map[string]any{admitBody(reqID, amount)}})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var batch struct {
			Items []admitVerdict `json:"items"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &batch))
		require.Len(t, batch.Items, 1)
		return batch.Items[0]
	}

	// 1) Admit 600 → 200 allowed (places the Redis hold).
	r1 := requestPrefix + "-r1"
	r2 := requestPrefix + "-r2"
	r3 := requestPrefix + "-r3"
	r4 := requestPrefix + "-r4"

	a1 := admit(r1, 600)
	require.Equal(t, http.StatusOK, a1.Status)
	require.True(t, a1.Result.Allowed)

	// 2) Admit another 600 → 402: only 400 spendable after the 600 in-flight hold.
	a2 := admit(r2, 600)
	require.Equal(t, http.StatusPaymentRequired, a2.Status)
	require.False(t, a2.Result.Allowed)
	require.Equal(t, "money", a2.Result.BlockedBy)

	// 3) Capture r1 at 500 (actual < estimate) → 200; durable ledger debit lands.
	w := post("/v1/merchant/admissions/"+r1+"/capture", map[string]any{"amount": 500})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"Replayed":false`, "first capture moved the money")
	bal, err := ms.GetBalanceForCustomer(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(500), bal.Balance, "1000 − 500 captured = 500")

	// 3b) or#907: retry the capture over the wire with only fallback payer
	// coordinates. The first capture consumed the Redis pointer, so before
	// or#907 this exact retry (no admit_source echo) landed at a different
	// coordinate and debited again. Now the request id is the whole key:
	// 200, Replayed=true, balance unchanged.
	w = post("/v1/merchant/admissions/"+r1+"/capture", map[string]any{
		"amount": 500, "customer_id": payerID.String(), "currency": money.DefaultCurrency, "invoker": "user:a",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"Replayed":true`, "the retry moved nothing and must say so")
	bal, err = ms.GetBalanceForCustomer(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(500), bal.Balance, "an at-least-once capture retry must not double debit")

	// 3c) A retry that CHANGES the amount is a caller bug: 409, nothing moves.
	w = post("/v1/merchant/admissions/"+r1+"/capture", map[string]any{
		"amount": 900, "customer_id": payerID.String(), "currency": money.DefaultCurrency, "invoker": "user:a",
	})
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "idempotency_key_reused")
	bal, err = ms.GetBalanceForCustomer(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(500), bal.Balance)

	// 4) After capture freed the hold, a 300 admit fits the remaining 500 → 200.
	a3 := admit(r3, 300)
	require.Equal(t, http.StatusOK, a3.Status)
	require.True(t, a3.Result.Allowed, "the r1 hold was freed at capture, so 300 fits in 500")

	// 5) Release r3 → 200; the hold is freed (no charge), so a fresh 500 admit fits.
	w = post("/v1/merchant/admissions/"+r3+"/release", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	a4 := admit(r4, 500)
	require.Equal(t, http.StatusOK, a4.Status)
	require.True(t, a4.Result.Allowed, "release freed r3's hold, so the full 500 is spendable again")
}
