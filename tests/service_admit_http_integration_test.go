//go:build integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httproutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

// TestServiceAdmit_HTTP_EndToEnd drives the #513 spendgate admission path through
// the REAL HTTP service routes (POST /v1/service/admit + /credits/holds/:id/
// capture|release) against the suite's live Postgres + Redis — the full
// route → handler → Service.Admit → spendgate stack, not the in-process layer.
func TestServiceAdmit_HTTP_EndToEnd(t *testing.T) {
	suite := getSharedTestSuite(t)
	require.NotNil(t, suite.App.Runtime.RedisClient, "admit needs Redis")
	ctx := dbtest.WithTestMerchant(context.Background())

	userID := uuid.NewString()
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
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ginmw.ResolveMerchant(dbtest.TestMerchantID))
	group := router.Group("/v1/service")
	resolver := stubServiceTokenResolver{
		permissions: []string{controlplane.PermCreditsRead, controlplane.PermCreditsWrite, controlplane.PermCreditsSpend},
		resources: []authcore.ServiceTokenResource{
			controlplane.MerchantResource(dbtest.TestMerchantID),
			controlplane.CustomerResource(payerID),
		},
	}
	httproutes.RegisterServiceRoutes(group, suite.App.Runtime, ginmw.ServiceTokenRequired(resolver))

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

	// 1) Admit 600 → 200 allowed (places the Redis hold).
	w := post("/v1/service/admit", admitBody("r1", 600))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var a1 admitResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &a1))
	require.True(t, a1.Allowed)

	// 2) Admit another 600 → 402: only 400 spendable after the 600 in-flight hold.
	w = post("/v1/service/admit", admitBody("r2", 600))
	require.Equal(t, http.StatusPaymentRequired, w.Code, w.Body.String())
	var a2 admitResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &a2))
	require.False(t, a2.Allowed)
	require.Equal(t, "money", a2.BlockedBy)

	// 3) Capture r1 at 500 (actual < estimate) → 200; durable ledger debit lands.
	w = post("/v1/service/credits/holds/r1/capture", map[string]any{"amount": 500})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	bal, err := ms.GetBalanceForCustomer(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(500), bal.Balance, "1000 − 500 captured = 500")

	// 4) After capture freed the hold, a 300 admit fits the remaining 500 → 200.
	w = post("/v1/service/admit", admitBody("r3", 300))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var a3 admitResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &a3))
	require.True(t, a3.Allowed, "the r1 hold was freed at capture, so 300 fits in 500")

	// 5) Release r3 → 200; the hold is freed (no charge), so a fresh 500 admit fits.
	w = post("/v1/service/credits/holds/r3/release", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = post("/v1/service/admit", admitBody("r4", 500))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var a4 admitResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &a4))
	require.True(t, a4.Allowed, "release freed r3's hold, so the full 500 is spendable again")
}
