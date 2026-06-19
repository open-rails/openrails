//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	ginroutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
	"github.com/open-rails/openrails/pkg/merchant"
)

// newHostSeamAdminRouter mounts the #528 delegated /v1/admin surface with a
// host-seam delegated principal carrying the given merchant permissions. This is
// the migration pattern for the admin integration suite: the per-user admin-JWT
// model is retired, so admin tests authenticate as a delegated merchant principal
// (openrails:merchant:*), exactly like the host-app issuer→owner token does in
// production. (Reuses hostSeamAuthenticator from self_account_host_principal_test.)
func newHostSeamAdminRouter(t *testing.T, suite *TestContainerSuite, subject string, perms []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	group := e.Group("/v1/admin")
	ginroutes.RegisterAdminRoutes(group, suite.App.Runtime,
		ginmw.DelegatedPrincipalRequired(hostSeamAuthenticator{subject: subject, perms: perms}))
	return e
}

// TestAdminUserDetailComposite_Delegated validates the #528 delegated /v1/admin
// surface end-to-end against Postgres: a merchant admin holding
// openrails:merchant:billing:read reads a user's COMPOSITE billing detail, which
// embeds the entitlements section (plus the payment_methods + product_access
// sections introduced in #528 increment 4).
func TestAdminUserDetailComposite_Delegated(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "b3333333-3333-4333-8333-333333333333",
		[]string{controlplane.PermMerchantBillingRead})

	userID := uuid.New().String()
	now := time.Now().UTC()
	endAt := now.Add(30 * 24 * time.Hour)
	adminSourceID := uuid.New()
	// The RLS-aware Runtime.DB insert needs the merchant pinned on the context.
	mctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	suite.InsertEntitlement(mctx, &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  suite.ensureCustomer(mctx, userID),
		Entitlement: "premium",
		StartAt:     now.Add(-time.Second),
		EndAt:       &endAt,
		SourceID:    &adminSourceID,
		SourceType:  models.EntitlementSourceAdmin,
		CreatedAt:   now,
		UpdatedAt:   now,
	})

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/admin/users/%s", userID), nil)
	req.Header.Set("Authorization", "Bearer host-credential")
	w := httptest.NewRecorder()
	admin.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	require.Equal(t, userID, resp["customer_id"])
	ents, ok := resp["entitlements"].([]any)
	require.True(t, ok, "composite must embed entitlements: %s", w.Body.String())
	require.Len(t, ents, 1, "the seeded entitlement should appear in the composite")
	_, ok = resp["payment_methods"]
	require.True(t, ok, "composite must embed a payment_methods section")
	_, ok = resp["product_access"]
	require.True(t, ok, "composite must embed a product_access section")
}

// TestAdminSurface_RejectsWithoutDelegatedPrincipal confirms the delegated /admin
// gate fails closed: a delegated principal that lacks billing:read cannot read
// the composite (403), proving the surface is gated, not open.
func TestAdminSurface_RejectsWithoutBillingRead(t *testing.T) {
	suite := getSharedTestSuite(t)
	// A principal with only a write scope, missing billing:read.
	admin := newHostSeamAdminRouter(t, suite, "b4444444-4444-4444-8444-444444444444",
		[]string{controlplane.PermMerchantSubscriptionsWrite})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/users/"+uuid.New().String(), nil)
	req.Header.Set("Authorization", "Bearer host-credential")
	w := httptest.NewRecorder()
	admin.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
