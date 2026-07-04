//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprouter "github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// stubServiceCredentialResolver is a test resolver (issue #222) that accepts any non-empty
// bearer token as a valid API key for the test merchant with the given
// permissions, so the parity test can exercise the service-credential-authenticated
// public service routes without standing up a full AuthKit control plane.
type stubServiceCredentialResolver struct {
	permissions []string
	serviceJWT  bool
}

func (s stubServiceCredentialResolver) LooksLikeAPIKey(token string) bool {
	return token != "" && !s.serviceJWT
}

func (s stubServiceCredentialResolver) ResolveAPIKey(_ context.Context, token string) (*controlplane.ResolvedServiceCredential, error) {
	if token == "" || s.serviceJWT {
		return nil, authkit.ErrInvalidAccessToken
	}
	return s.resolved()
}

func (s stubServiceCredentialResolver) ResolveServiceJWT(_ context.Context, token string) (*controlplane.ResolvedServiceCredential, error) {
	if token == "" || !s.serviceJWT {
		return nil, authkit.ErrInvalidServiceJWT
	}
	return s.resolved()
}

func (s stubServiceCredentialResolver) resolved() (*controlplane.ResolvedServiceCredential, error) {
	return &controlplane.ResolvedServiceCredential{
		OwnerGroupRef: dbtest.TestMerchantSlug,
		MerchantID:    dbtest.TestMerchantID,
		MerchantSlug:  dbtest.TestMerchantSlug,
		Permissions:   s.permissions,
	}, nil
}

func TestServiceFacade_CreditsAndEntitlements_ParityWithServiceHTTP(t *testing.T) {
	suite := getSharedTestSuite(t)
	// In-process facade calls require the merchant pinned in context (the raw
	// Service has no default merchant; the HTTP path below pins it via middleware).
	ctx := dbtest.WithTestMerchant(context.Background())

	userID := uuid.NewString()
	tenantSubjectID := suite.ensureCustomer(ctx, userID)
	tenantSubject := identity.CustomerID(tenantSubjectID)

	// Seed a starting money balance (USD) via the #512 ledger deposit (the
	// single-entry money_blocks table was dropped in the hard cut; balance is now
	// derived from ledger_accounts).
	_, err := money.NewMoneyService(suite.App.Runtime.DB).Deposit(ctx, money.DepositParams{
		CustomerID: &tenantSubject, Invoker: tenantSubjectID.String(),
		Currency: money.DefaultCurrency, Amount: 10_000, Source: "seed",
	})
	require.NoError(t, err)

	// Seed an entitlement. source_id is NOT NULL (the originating
	// subscription/payment/admin-grant id); use a synthetic admin source here.
	entSourceID := uuid.New()
	ent := &models.Entitlement{
		ID:          uuid.New(),
		MerchantID:  dbtest.TestMerchantID.UUID(),
		CustomerID:  tenantSubjectID,
		Entitlement: "premium-1",
		StartAt:     time.Now().Add(-1 * time.Hour).UTC(),
		EndAt:       nil,
		SourceID:    &entSourceID,
		SourceType:  models.EntitlementSourceAdmin,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	suite.InsertEntitlement(ctx, ent)
	legacyOnlyCustomerID := uuid.New()
	_, err = suite.Pool.Exec(ctx, `
		INSERT INTO openrails.customers (id, merchant_id, issuer, subject, created_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		legacyOnlyCustomerID, dbtest.TestMerchantID.UUID(),
		"service-facade-parity-other", userID+"-other",
		time.Now().UTC(), time.Now().UTC())
	require.NoError(t, err)
	legacyOnlyEnt := &models.Entitlement{
		ID:          uuid.New(),
		MerchantID:  dbtest.TestMerchantID.UUID(),
		CustomerID:  legacyOnlyCustomerID,
		Entitlement: "legacy-user-only",
		StartAt:     time.Now().Add(-1 * time.Hour).UTC(),
		EndAt:       nil,
		SourceID:    &entSourceID,
		SourceType:  models.EntitlementSourceAdmin,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	suite.InsertEntitlement(ctx, legacyOnlyEnt)

	// Build the in-process facade.
	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	// Build the service-credential-authenticated PUBLIC service routes (issue #222)
	// directly, with a stub resolver granting the credit + entitlement permissions. This
	// is the same surface registered on the public handler at /v1/merchant/*.
	mux := http.NewServeMux()
	resolver := stubServiceCredentialResolver{
		permissions: []string{
			controlplane.PermMerchantCustomerSettingsRead,
			controlplane.PermMerchantCustomerSettingsUpdate,
			controlplane.PermMerchantAdmissionsCreate,
			controlplane.PermMerchantCustomerSettingsRead,
		},
	}
	httproutes.RegisterServiceRoutes(httprouter.NewMux(mux, "/v1/merchant", suite.App.Runtime), suite.App.Runtime, httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{ServiceCredentialResolver: resolver})})
	router := middleware.ChainHTTP(mux, middleware.ResolveMerchantHTTP(middleware.StaticMerchant(dbtest.TestMerchantID)))

	withServiceCredential := func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer openrails_st_testkeyid_testsecret")
	}

	// Hold create/capture/release parity moved to the spendgate admit path (#513);
	// the legacy /credits/hold + HoldCredits facade were removed. Capture/release
	// parity over the unified admit gate is covered by the spendgate + admitter
	// integration tests.

	// 1) Entitlements: facade and service HTTP both see the record.
	ents, err := svc.ListActiveEntitlements(ctx, userID, time.Now().UTC())
	require.NoError(t, err)
	require.Contains(t, ents, "premium-1")
	subjectEnts, err := svc.ListActiveEntitlementsForCustomer(ctx, tenantSubject, time.Now().UTC())
	require.NoError(t, err)
	require.Contains(t, subjectEnts, "premium-1")

	reqEnt := httptest.NewRequest(http.MethodGet, "/v1/merchant/customers/"+tenantSubjectID.String()+"/entitlements", nil)
	withServiceCredential(reqEnt)
	wEnt := httptest.NewRecorder()
	router.ServeHTTP(wEnt, reqEnt)
	require.Equal(t, http.StatusOK, wEnt.Code)
	var entResp []struct {
		ID          string `json:"id"`
		CustomerID  string `json:"customer_id"`
		UserID      string `json:"user_id"`
		Entitlement string `json:"entitlement"`
	}
	require.NoError(t, json.Unmarshal(wEnt.Body.Bytes(), &entResp))
	require.Len(t, entResp, 1)
	require.Equal(t, tenantSubjectID.String(), entResp[0].CustomerID)
	require.Empty(t, entResp[0].UserID)
	require.Equal(t, "premium-1", entResp[0].Entitlement)
	require.NotContains(t, wEnt.Body.String(), "legacy-user-only")

	// 4) The same real service routes accept a resolved first-party service JWT
	// principal; no OpenRails-generated runtime token is needed on these paths.
	jwtMux := http.NewServeMux()
	jwtResolver := resolver
	jwtResolver.serviceJWT = true
	httproutes.RegisterServiceRoutes(httprouter.NewMux(jwtMux, "/v1/merchant", suite.App.Runtime), suite.App.Runtime, httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{ServiceCredentialResolver: jwtResolver})})
	jwtRouter := middleware.ChainHTTP(jwtMux, middleware.ResolveMerchantHTTP(middleware.StaticMerchant(dbtest.TestMerchantID)))
	withServiceJWT := func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer eyJ.service.jwt")
	}

	reqJWTEnt := httptest.NewRequest(http.MethodGet, "/v1/merchant/customers/"+tenantSubjectID.String()+"/entitlements", nil)
	withServiceJWT(reqJWTEnt)
	wJWTEnt := httptest.NewRecorder()
	jwtRouter.ServeHTTP(wJWTEnt, reqJWTEnt)
	require.Equal(t, http.StatusOK, wJWTEnt.Code)
	require.Contains(t, wJWTEnt.Body.String(), "premium-1")

	reqJWTBalance := httptest.NewRequest(http.MethodGet, "/v1/merchant/credits/balance?customer_id="+tenantSubjectID.String()+"&currency="+money.DefaultCurrency, nil)
	withServiceJWT(reqJWTBalance)
	wJWTBalance := httptest.NewRecorder()
	jwtRouter.ServeHTTP(wJWTBalance, reqJWTBalance)
	require.Equal(t, http.StatusOK, wJWTBalance.Code)
	var balanceResp struct {
		CustomerID    string `json:"customer_id"`
		BalanceAmount int64  `json:"balance_amount"`
		HeldAmount    int64  `json:"held_amount"`
	}
	require.NoError(t, json.Unmarshal(wJWTBalance.Body.Bytes(), &balanceResp))
	require.Equal(t, tenantSubjectID.String(), balanceResp.CustomerID)
	require.Equal(t, int64(10_000), balanceResp.BalanceAmount, "the full seed; no spend remains in this parity test")
	require.Equal(t, int64(0), balanceResp.HeldAmount)
}
