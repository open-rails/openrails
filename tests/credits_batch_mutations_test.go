//go:build integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/server"
)

func TestCredits_BatchHoldWithdrawCaptureRelease(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	userID := uuid.NewString()

	// Seed credit types (shared suite means these may already exist).
	_, err := suite.BunDB.ExecContext(ctx,
		`INSERT INTO billing.credit_types (id, name, display_name, unit, decimal_places, is_active, created_at)
		 VALUES (?::uuid, 'api_credits', 'API Credits', 'credits', 0, true, NOW())
		 ON CONFLICT (name) DO NOTHING`,
		uuid.NewString(),
	)
	require.NoError(t, err)
	_, err = suite.BunDB.ExecContext(ctx,
		`INSERT INTO billing.credit_types (id, name, display_name, unit, decimal_places, is_active, created_at)
		 VALUES (?::uuid, 'other_credits', 'Other Credits', 'credits', 0, true, NOW())
		 ON CONFLICT (name) DO NOTHING`,
		uuid.NewString(),
	)
	require.NoError(t, err)

	// Private/service HTTP handler (same runtime, APIKey enabled).
	cfg2 := *suite.Config
	cfg2.APIKey = "test-service-key"
	privateSrv, err := server.New(server.Dependencies{
		Config:       (*config.Config)(&cfg2),
		Cache:        suite.App.Cache,
		Runtime:      suite.App.Runtime,
		Redis:        suite.App.RedisClient,
		AuthProvider: suite.App.AuthProvider,
	})
	require.NoError(t, err)
	privateHandler := privateSrv.PrivateHandler()

	do := func(method, path string, body any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&buf).Encode(body))
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(buf.Bytes()))
		req.Header.Set("X-API-KEY", cfg2.APIKey)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		privateHandler.ServeHTTP(w, req)
		return w
	}

	// Seed balance via deposit (creates blocks too).
	wDep := do(http.MethodPost, "/v1/credits/deposit", map[string]any{
		"user_id":     userID,
		"credit_type": "api_credits",
		"amount":      100,
		"source":      "test",
	})
	require.Equal(t, http.StatusOK, wDep.Code, wDep.Body.String())

	// Batch withdraw: one success, one insufficient.
	wW := do(http.MethodPost, "/v1/credits/withdraw", []map[string]any{
		{
			"user_id":     userID,
			"credit_type": "api_credits",
			"amount":      10,
			"source":      "test",
		},
		{
			"user_id":     userID,
			"credit_type": "api_credits",
			"amount":      10_000,
			"source":      "test",
		},
		{
			// Invalid item: missing user_id.
			"credit_type": "api_credits",
			"amount":      1,
			"source":      "test",
		},
	})
	require.Equal(t, http.StatusOK, wW.Code, wW.Body.String())
	var outW struct {
		Results []struct {
			Ok     bool   `json:"ok"`
			Status int    `json:"status"`
			Error  string `json:"error"`
			Result *struct {
				Amount int64 `json:"amount"`
			} `json:"result"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(wW.Body.Bytes(), &outW))
	require.Len(t, outW.Results, 3)
	require.True(t, outW.Results[0].Ok)
	require.Equal(t, http.StatusOK, outW.Results[0].Status)
	require.NotNil(t, outW.Results[0].Result)
	require.Equal(t, int64(-10), outW.Results[0].Result.Amount)
	require.False(t, outW.Results[1].Ok)
	require.Equal(t, http.StatusPaymentRequired, outW.Results[1].Status)
	require.Equal(t, "insufficient_credits", outW.Results[1].Error)
	require.False(t, outW.Results[2].Ok)
	require.Equal(t, http.StatusBadRequest, outW.Results[2].Status)
	require.Equal(t, "invalid_request", outW.Results[2].Error)

	// Batch hold (2 holds).
	expiresAt := time.Now().Add(10 * time.Minute).Unix()
	wH := do(http.MethodPost, "/v1/credits/hold", []map[string]any{
		{
			"user_id":     userID,
			"credit_type": "api_credits",
			"amount":      5,
			"source":      "test",
			"source_id":   "h1",
			"expires_at":  expiresAt,
		},
		{
			"user_id":     userID,
			"credit_type": "api_credits",
			"amount":      6,
			"source":      "test",
			"source_id":   "h2",
			"expires_at":  expiresAt,
		},
	})
	require.Equal(t, http.StatusOK, wH.Code, wH.Body.String())
	var outH struct {
		Results []struct {
			Ok     bool   `json:"ok"`
			Status int    `json:"status"`
			Error  string `json:"error"`
			Result *struct {
				ID string `json:"id"`
			} `json:"result"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(wH.Body.Bytes(), &outH))
	require.Len(t, outH.Results, 2)
	require.True(t, outH.Results[0].Ok)
	require.True(t, outH.Results[1].Ok)
	hold1, err := uuid.Parse(outH.Results[0].Result.ID)
	require.NoError(t, err)
	hold2, err := uuid.Parse(outH.Results[1].Result.ID)
	require.NoError(t, err)

	// Batch capture both holds.
	wC := do(http.MethodPost, "/v1/credits/holds/batch/capture", []map[string]any{
		{"hold_id": hold1, "amount": 5},
		{"hold_id": hold2, "amount": 6},
	})
	require.Equal(t, http.StatusOK, wC.Code, wC.Body.String())
	var outC struct {
		Results []struct {
			Ok     bool   `json:"ok"`
			Status int    `json:"status"`
			Error  string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(wC.Body.Bytes(), &outC))
	require.Len(t, outC.Results, 2)
	require.True(t, outC.Results[0].Ok)
	require.True(t, outC.Results[1].Ok)

	// Batch release: create two more holds and release them.
	wH2 := do(http.MethodPost, "/v1/credits/hold", []map[string]any{
		{
			"user_id":     userID,
			"credit_type": "api_credits",
			"amount":      1,
			"source":      "test",
			"source_id":   "h3",
			"expires_at":  expiresAt,
		},
		{
			"user_id":     userID,
			"credit_type": "api_credits",
			"amount":      1,
			"source":      "test",
			"source_id":   "h4",
			"expires_at":  expiresAt,
		},
	})
	require.Equal(t, http.StatusOK, wH2.Code, wH2.Body.String())
	require.NoError(t, json.Unmarshal(wH2.Body.Bytes(), &outH))
	hold3, err := uuid.Parse(outH.Results[0].Result.ID)
	require.NoError(t, err)
	hold4, err := uuid.Parse(outH.Results[1].Result.ID)
	require.NoError(t, err)

	wR := do(http.MethodPost, "/v1/credits/holds/batch/release", []map[string]any{
		{"hold_id": hold3},
		{"hold_id": hold4},
	})
	require.Equal(t, http.StatusOK, wR.Code, wR.Body.String())
	var outR struct {
		Results []struct {
			Ok     bool   `json:"ok"`
			Status int    `json:"status"`
			Error  string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(wR.Body.Bytes(), &outR))
	require.Len(t, outR.Results, 2)
	require.True(t, outR.Results[0].Ok)
	require.True(t, outR.Results[1].Ok)
}

func TestCredits_AllowNegative_BoundedAndRestrictedToAPICredits(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	userID := uuid.NewString()

	_, err := suite.BunDB.ExecContext(ctx,
		`INSERT INTO billing.credit_types (id, name, display_name, unit, decimal_places, is_active, created_at)
		 VALUES (?::uuid, 'api_credits', 'API Credits', 'credits', 0, true, NOW())
		 ON CONFLICT (name) DO NOTHING`,
		uuid.NewString(),
	)
	require.NoError(t, err)
	_, err = suite.BunDB.ExecContext(ctx,
		`INSERT INTO billing.credit_types (id, name, display_name, unit, decimal_places, is_active, created_at)
		 VALUES (?::uuid, 'other_credits', 'Other Credits', 'credits', 0, true, NOW())
		 ON CONFLICT (name) DO NOTHING`,
		uuid.NewString(),
	)
	require.NoError(t, err)

	cfg2 := *suite.Config
	cfg2.APIKey = "test-service-key"
	privateSrv, err := server.New(server.Dependencies{
		Config:       (*config.Config)(&cfg2),
		Cache:        suite.App.Cache,
		Runtime:      suite.App.Runtime,
		Redis:        suite.App.RedisClient,
		AuthProvider: suite.App.AuthProvider,
	})
	require.NoError(t, err)
	privateHandler := privateSrv.PrivateHandler()

	do := func(method, path string, body any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&buf).Encode(body))
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(buf.Bytes()))
		req.Header.Set("X-API-KEY", cfg2.APIKey)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		privateHandler.ServeHTTP(w, req)
		return w
	}

	// Bounded negative allowed for api_credits.
	wOk := do(http.MethodPost, "/v1/credits/withdraw", map[string]any{
		"user_id":        userID,
		"credit_type":    "api_credits",
		"amount":         5,
		"source":         "test",
		"allow_negative": true,
		"max_negative":   10,
	})
	require.Equal(t, http.StatusOK, wOk.Code, wOk.Body.String())

	// Exceeding max_negative is rejected.
	wTooFar := do(http.MethodPost, "/v1/credits/withdraw", map[string]any{
		"user_id":        userID,
		"credit_type":    "api_credits",
		"amount":         100,
		"source":         "test",
		"allow_negative": true,
		"max_negative":   10,
	})
	require.Equal(t, http.StatusPaymentRequired, wTooFar.Code, wTooFar.Body.String())
	require.Contains(t, wTooFar.Body.String(), "negative_balance_limit_exceeded")

	// Not allowed for other credit types.
	wBad := do(http.MethodPost, "/v1/credits/withdraw", map[string]any{
		"user_id":        userID,
		"credit_type":    "other_credits",
		"amount":         1,
		"source":         "test",
		"allow_negative": true,
		"max_negative":   10,
	})
	require.Equal(t, http.StatusBadRequest, wBad.Code, wBad.Body.String())
	require.Contains(t, wBad.Body.String(), "invalid_negative_policy")
}
