//go:build integration

package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #511: a manual admin entitlement grant is now an `admin`-sourced entry in the
// grant ledger (the entitlement is the projected effect) — there is no separate
// entitlement_grants provenance row anymore. #528 hard cut: the grant is
// authorized by the delegated merchant:customer-settings:update capability.
func TestAdminEntitlementGrantCreatesEntitlement(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "b8888888-8888-4888-8888-888888888888",
		[]string{controlplane.PermMerchantCustomerSettingsUpdate})

	userID := uuid.New().String()

	body, err := json.Marshal(map[string]any{
		"entitlement": "premium",
		"days":        7,
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/merchant/customers/"+userID+"/entitlements", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
	req.Header.Set("Content-Type", "application/json")
	admin.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var ent models.Entitlement
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &ent))

	require.Equal(t, userID, ent.CustomerID.String())
	require.Equal(t, "premium", ent.Entitlement)
	require.Equal(t, models.EntitlementSourceAdmin, ent.SourceType)
	require.NotNil(t, ent.SourceID)
}

// TestAdminEntitlementGrant_RequiresEntitlementsWrite proves the delegated gate
// fails closed: a principal with only billing:read cannot grant an entitlement.
func TestAdminEntitlementGrant_RequiresEntitlementsWrite(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "b9999999-9999-4999-8999-999999999999",
		[]string{controlplane.PermMerchantCustomerSettingsRead})

	body, _ := json.Marshal(map[string]any{"entitlement": "premium", "days": 7})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/merchant/customers/"+uuid.New().String()+"/entitlements", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
	req.Header.Set("Content-Type", "application/json")
	admin.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

func TestRemovedEntitlementGrantRoutesReturn404(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "ba000000-0000-4000-8000-000000000000",
		[]string{controlplane.PermMerchantCustomerSettingsUpdate})

	userID := uuid.New().String()

	for _, path := range []string{
		"/v1/merchant/customers/" + userID + "/grants",
		"/v1/merchant/grants/" + uuid.New().String(),
	} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
		admin.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code, path)
	}
}

func TestAdminEntitlementAppendsAfterLatestEnd(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "ba111111-1111-4111-8111-111111111111",
		[]string{controlplane.PermMerchantCustomerSettingsUpdate})

	userID := uuid.New().String()
	fixedNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	suite.SetMockClock(fixedNow)

	// The RLS-aware Runtime.DB insert needs the merchant pinned on the context.
	ctx := dbtest.WithTestMerchant(t.Context())

	// Create a subscription-sourced entitlement window that ends in the future.
	subID := uuid.New()
	start := fixedNow
	subEnd := fixedNow.Add(30 * 24 * time.Hour)
	existing := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  suite.ensureCustomer(ctx, userID),
		Entitlement: "premium",
		StartAt:     start,
		EndAt:       &subEnd,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &subID,
		CreatedAt:   fixedNow,
		UpdatedAt:   fixedNow,
	}
	suite.InsertEntitlement(ctx, existing)

	body, err := json.Marshal(map[string]any{
		"entitlement": "premium",
		"days":        7,
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/merchant/customers/"+userID+"/entitlements", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
	req.Header.Set("Content-Type", "application/json")
	admin.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var created models.Entitlement
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	require.Equal(t, models.EntitlementSourceAdmin, created.SourceType)
	require.NotNil(t, created.SourceID)
	// WithinDuration instead of Equal: these times cross a DB + JSON round
	// trip, which can change the time.Time internal representation (location
	// pointer) even when the instants are identical.
	require.WithinDuration(t, subEnd, created.StartAt, 0)
	require.NotNil(t, created.EndAt)
	require.WithinDuration(t, subEnd.Add(7*24*time.Hour), *created.EndAt, 0)
}
