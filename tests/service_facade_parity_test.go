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

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httproutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
	"github.com/open-rails/openrails/pkg/tenant"
)

// stubServiceTokenResolver is a test resolver (issue #222) that accepts any non-empty
// bearer token as a valid service token for the default tenant with the given
// permissions, so the parity test can exercise the service-token-authenticated
// public service routes without standing up a full AuthKit control plane.
type stubServiceTokenResolver struct {
	permissions []string
	resources   []authcore.ServiceTokenResource
	serviceJWT  bool
}

func (s stubServiceTokenResolver) LooksLikeServiceToken(token string) bool {
	return token != "" && !s.serviceJWT
}

func (s stubServiceTokenResolver) ResolveServiceToken(_ context.Context, token string) (*controlplane.ResolvedServiceToken, error) {
	if token == "" || s.serviceJWT {
		return nil, authcore.ErrInvalidAccessToken
	}
	return s.resolved()
}

func (s stubServiceTokenResolver) ResolveServiceJWT(_ context.Context, token string) (*controlplane.ResolvedServiceToken, error) {
	if token == "" || !s.serviceJWT {
		return nil, authcore.ErrInvalidServiceJWT
	}
	return s.resolved()
}

func (s stubServiceTokenResolver) resolved() (*controlplane.ResolvedServiceToken, error) {
	return &controlplane.ResolvedServiceToken{
		AuthKitTenantSlug: "operator",
		TenantID:          tenant.DefaultID,
		TenantSlug:        tenant.DefaultSlug,
		Permissions:       s.permissions,
		Resources:         s.resources,
	}, nil
}

func TestServiceFacade_CreditsAndEntitlements_ParityWithServiceHTTP(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	userID := uuid.NewString()
	tenantSubjectID := suite.ensureTenantSubject(ctx, userID)
	tenantSubject := identity.TenantSubjectID(tenantSubjectID)
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
	_, err := suite.Pool.Exec(ctx, `
		INSERT INTO billing.credit_types (id, name, display_name, unit, decimal_places, is_active, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		ct.ID, ct.Name, ct.DisplayName, ct.Unit, ct.DecimalPlaces, ct.IsActive, ct.CreatedAt)
	require.NoError(t, err)

	ucb := &models.CreditBalance{
		ID:              uuid.New(),
		TenantID:        tenant.DefaultID.UUID(),
		TenantSubjectID: tenantSubjectID,
		CreditTypeID:    ct.ID,
		Balance:         10_000,
		HeldBalance:     0,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	_, err = suite.Pool.Exec(ctx, `
		INSERT INTO billing.credit_balances (id, tenant_id, tenant_subject_id, credit_type_id, balance, held_balance, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		ucb.ID, ucb.TenantID, ucb.TenantSubjectID, ucb.CreditTypeID, ucb.Balance, ucb.HeldBalance, ucb.CreatedAt, ucb.UpdatedAt)
	require.NoError(t, err)

	// Seed an entitlement. source_id is NOT NULL (the originating
	// subscription/payment/admin-grant id); use a synthetic admin source here.
	entSourceID := uuid.New()
	ent := &models.Entitlement{
		ID:              uuid.New(),
		TenantID:        tenant.DefaultID.UUID(),
		TenantSubjectID: tenantSubjectID,
		Entitlement:     "premium-1",
		StartAt:         time.Now().Add(-1 * time.Hour).UTC(),
		EndAt:           nil,
		SourceID:        &entSourceID,
		SourceType:      models.EntitlementSourceAdmin,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	suite.InsertEntitlement(ctx, ent)
	legacyOnlyTenantSubjectID := uuid.New()
	_, err = suite.Pool.Exec(ctx, `
		INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject, created_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		legacyOnlyTenantSubjectID, tenant.DefaultID.UUID(),
		"service-facade-parity-other", userID+"-other",
		time.Now().UTC(), time.Now().UTC())
	require.NoError(t, err)
	legacyOnlyEnt := &models.Entitlement{
		ID:              uuid.New(),
		TenantID:        tenant.DefaultID.UUID(),
		TenantSubjectID: legacyOnlyTenantSubjectID,
		Entitlement:     "legacy-user-only",
		StartAt:         time.Now().Add(-1 * time.Hour).UTC(),
		EndAt:           nil,
		SourceID:        &entSourceID,
		SourceType:      models.EntitlementSourceAdmin,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	suite.InsertEntitlement(ctx, legacyOnlyEnt)

	// Build the in-process facade.
	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	// Build the service-token-authenticated PUBLIC service routes (issue #222)
	// directly, with a stub resolver granting the credit + entitlement permissions. This
	// is the same surface registered on the public handler at /v1/service/*.
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ginmw.ResolveTenant())
	group := router.Group("/v1/service")
	resolver := stubServiceTokenResolver{
		permissions: []string{
			controlplane.PermCreditsRead,
			controlplane.PermCreditsWrite,
			controlplane.PermCreditsSpend,
			controlplane.PermEntitlementsRead,
		},
		resources: []authcore.ServiceTokenResource{
			controlplane.TenantResource(tenant.DefaultID),
			controlplane.TenantSubjectResource(tenantSubjectID),
		},
	}
	httproutes.RegisterServiceRoutes(group, suite.App.Runtime, ginmw.ServiceTokenRequired(resolver), nil)

	withServiceToken := func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer openrails_st_testkeyid_testsecret")
	}

	// 1) Create hold via Service facade, release via service HTTP.
	hold1, err := svc.HoldCredits(ctx, billingservice.HoldCreditsRequest{
		TenantSubjectID: &tenantSubject,
		Actor:           userID,
		CreditType:      creditTypeName,
		Amount:          123,
		Source:          "svc_test",
		SourceID:        "hold-1",
		ExpiresAt:       time.Now().Add(10 * time.Minute).UTC(),
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, hold1.ID)

	reqRelease := httptest.NewRequest(http.MethodPost, "/v1/service/credits/hold/"+hold1.ID.String()+"/release", nil)
	withServiceToken(reqRelease)
	wRelease := httptest.NewRecorder()
	router.ServeHTTP(wRelease, reqRelease)
	require.Equal(t, http.StatusOK, wRelease.Code)

	// 2) Create hold via service HTTP, capture via Service facade.
	bodyHold, _ := json.Marshal(map[string]any{
		"tenant_subject_id": tenantSubjectID.String(),
		"actor":             userID,
		"credit_type":       creditTypeName,
		"amount":            456,
		"source":            "svc_test",
		"source_id":         "hold-2",
		"expires_at":        time.Now().Add(10 * time.Minute).Unix(),
	})
	reqHold := httptest.NewRequest(http.MethodPost, "/v1/service/credits/hold", bytes.NewReader(bodyHold))
	withServiceToken(reqHold)
	reqHold.Header.Set("Content-Type", "application/json")
	wHold := httptest.NewRecorder()
	router.ServeHTTP(wHold, reqHold)
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
	subjectEnts, err := svc.ListActiveEntitlementsForTenantSubject(ctx, tenantSubject, time.Now().UTC())
	require.NoError(t, err)
	require.Contains(t, subjectEnts, "premium-1")

	reqEnt := httptest.NewRequest(http.MethodGet, "/v1/service/tenant-subjects/"+tenantSubjectID.String()+"/entitlements", nil)
	withServiceToken(reqEnt)
	wEnt := httptest.NewRecorder()
	router.ServeHTTP(wEnt, reqEnt)
	require.Equal(t, http.StatusOK, wEnt.Code)
	var entResp []struct {
		ID              string `json:"id"`
		TenantSubjectID string `json:"tenant_subject_id"`
		UserID          string `json:"user_id"`
		Entitlement     string `json:"entitlement"`
	}
	require.NoError(t, json.Unmarshal(wEnt.Body.Bytes(), &entResp))
	require.Len(t, entResp, 1)
	require.Equal(t, tenantSubjectID.String(), entResp[0].TenantSubjectID)
	require.Empty(t, entResp[0].UserID)
	require.Equal(t, "premium-1", entResp[0].Entitlement)
	require.NotContains(t, wEnt.Body.String(), "legacy-user-only")

	// 4) The same real service routes accept a resolved first-party service JWT
	// principal; no OpenRails-generated runtime token is needed on these paths.
	jwtRouter := gin.New()
	jwtRouter.Use(ginmw.ResolveTenant())
	jwtGroup := jwtRouter.Group("/v1/service")
	jwtResolver := resolver
	jwtResolver.serviceJWT = true
	httproutes.RegisterServiceRoutes(jwtGroup, suite.App.Runtime, ginmw.ServiceTokenRequired(jwtResolver), nil)
	withServiceJWT := func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer eyJ.service.jwt")
	}

	reqJWTEnt := httptest.NewRequest(http.MethodGet, "/v1/service/tenant-subjects/"+tenantSubjectID.String()+"/entitlements", nil)
	withServiceJWT(reqJWTEnt)
	wJWTEnt := httptest.NewRecorder()
	jwtRouter.ServeHTTP(wJWTEnt, reqJWTEnt)
	require.Equal(t, http.StatusOK, wJWTEnt.Code)
	require.Contains(t, wJWTEnt.Body.String(), "premium-1")

	reqJWTBalance := httptest.NewRequest(http.MethodGet, "/v1/service/credits/balance?tenant_subject_id="+tenantSubjectID.String()+"&credit_type="+creditTypeName, nil)
	withServiceJWT(reqJWTBalance)
	wJWTBalance := httptest.NewRecorder()
	jwtRouter.ServeHTTP(wJWTBalance, reqJWTBalance)
	require.Equal(t, http.StatusOK, wJWTBalance.Code)
	var balanceResp struct {
		TenantSubjectID string `json:"tenant_subject_id"`
		BalanceMicros   int64  `json:"balance_micros"`
		HeldMicros      int64  `json:"held_micros"`
	}
	require.NoError(t, json.Unmarshal(wJWTBalance.Body.Bytes(), &balanceResp))
	require.Equal(t, tenantSubjectID.String(), balanceResp.TenantSubjectID)
	require.Equal(t, int64(9_889), balanceResp.BalanceMicros)
	require.Equal(t, int64(0), balanceResp.HeldMicros)
}
