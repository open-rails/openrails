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
	"github.com/open-rails/openrails/internal/db/models"
	server "github.com/open-rails/openrails/internal/http"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func TestServiceFacade_CreditsAndEntitlements_ParityWithServiceHTTP(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	userID := uuid.New().String()
	creditTypeName := "svc_test_credits_" + uuid.NewString()

	// Seed a credit type and starting balance.
	ct := &models.CreditType{
		ID:            uuid.New(),
		Name:          creditTypeName,
		DisplayName:   "Service Test Credits",
		Unit:          "USD",
		DecimalPlaces: 2,
		IsActive:      true,
		CreatedAt:     time.Now().UTC(),
	}
	_, err := suite.BunDB.NewInsert().Model(ct).Exec(ctx)
	require.NoError(t, err)

	ucb := &models.UserCreditBalance{
		ID:           uuid.New(),
		UserID:       userID,
		CreditTypeID: ct.ID,
		Balance:      10_000,
		HeldBalance:  0,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	_, err = suite.BunDB.NewInsert().Model(ucb).Exec(ctx)
	require.NoError(t, err)

	// Seed an entitlement.
	ent := &models.Entitlement{
		ID:          uuid.New(),
		UserID:      userID,
		Entitlement: "premium-1",
		StartAt:     time.Now().Add(-1 * time.Hour).UTC(),
		EndAt:       nil,
		SourceType:  models.EntitlementSourceAdmin,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_, err = suite.BunDB.NewInsert().Model(ent).Exec(ctx)
	require.NoError(t, err)

	// Build the in-process facade.
	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	// Build a private/service HTTP handler using the same runtime, but with APIKey enabled.
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

	// 1) Create hold via Service facade, release via service HTTP.
	hold1, err := svc.HoldCredits(ctx, billingservice.HoldCreditsRequest{
		UserID:     userID,
		CreditType: creditTypeName,
		Amount:     123,
		Source:     "svc_test",
		SourceID:   "hold-1",
		ExpiresAt:  time.Now().Add(10 * time.Minute).UTC(),
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, hold1.ID)

	reqRelease := httptest.NewRequest(http.MethodPost, "/v1/credits/hold/"+hold1.ID.String()+"/release", nil)
	reqRelease.Header.Set("X-API-KEY", cfg2.APIKey)
	wRelease := httptest.NewRecorder()
	privateHandler.ServeHTTP(wRelease, reqRelease)
	require.Equal(t, http.StatusOK, wRelease.Code)

	// 2) Create hold via service HTTP, capture via Service facade.
	bodyHold, _ := json.Marshal(map[string]any{
		"user_id":     userID,
		"credit_type": creditTypeName,
		"amount":      456,
		"source":      "svc_test",
		"source_id":   "hold-2",
		"expires_at":  time.Now().Add(10 * time.Minute).Unix(),
	})
	reqHold := httptest.NewRequest(http.MethodPost, "/v1/credits/hold", bytes.NewReader(bodyHold))
	reqHold.Header.Set("X-API-KEY", cfg2.APIKey)
	reqHold.Header.Set("Content-Type", "application/json")
	wHold := httptest.NewRecorder()
	privateHandler.ServeHTTP(wHold, reqHold)
	require.Equal(t, http.StatusOK, wHold.Code)

	var holdResp struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(wHold.Body.Bytes(), &holdResp))
	holdID, err := uuid.Parse(holdResp.ID)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, holdID)

	trx, err := svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{HoldID: holdID, Amount: 111})
	require.NoError(t, err)
	require.Equal(t, int64(-111), trx.Amount)

	// 3) Entitlements: facade and service HTTP both see the record.
	ents, err := svc.ListActiveEntitlements(ctx, userID, time.Now().UTC())
	require.NoError(t, err)
	require.Contains(t, ents, "premium-1")

	reqEnt := httptest.NewRequest(http.MethodGet, "/v1/users/"+userID+"/entitlements", nil)
	reqEnt.Header.Set("X-API-KEY", cfg2.APIKey)
	wEnt := httptest.NewRecorder()
	privateHandler.ServeHTTP(wEnt, reqEnt)
	require.Equal(t, http.StatusOK, wEnt.Code)
	require.Contains(t, wEnt.Body.String(), "premium-1")
}
