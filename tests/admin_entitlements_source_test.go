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

	"github.com/open-rails/openrails/internal/db/models"
)

// #511: a manual admin entitlement grant is now an `admin`-sourced entry in the
// grant ledger (the entitlement is the projected effect) — there is no separate
// entitlement_grants provenance row anymore.
func TestAdminEntitlementGrantCreatesEntitlement(t *testing.T) {
	suite, adminToken := setupAdminTestSuite(t)

	userID := uuid.New().String()

	body, err := json.Marshal(map[string]any{
		"entitlement": "premium",
		"days":        7,
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/admin/users/"+userID+"/entitlements", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")

	suite.Server.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var ent models.Entitlement
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &ent))

	require.Equal(t, userID, ent.CustomerID.String())
	require.Equal(t, "premium", ent.Entitlement)
	require.Equal(t, models.EntitlementSourceAdmin, ent.SourceType)
	require.NotNil(t, ent.SourceID)
}

func TestRemovedEntitlementGrantRoutesReturn404(t *testing.T) {
	suite, adminToken := setupAdminTestSuite(t)

	userID := uuid.New().String()

	for _, path := range []string{
		"/v1/admin/users/" + userID + "/grants",
		"/v1/admin/grants/" + uuid.New().String(),
	} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)

		suite.Server.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code, path)
	}
}

func TestAdminEntitlementAppendsAfterLatestEnd(t *testing.T) {
	suite, adminToken := setupAdminTestSuite(t)

	userID := uuid.New().String()
	fixedNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	suite.SetMockClock(fixedNow)

	// Create a subscription-sourced entitlement window that ends in the future.
	subID := uuid.New()
	start := fixedNow
	subEnd := fixedNow.Add(30 * 24 * time.Hour)
	existing := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  suite.ensureCustomer(context.Background(), userID),
		Entitlement: "premium",
		StartAt:     start,
		EndAt:       &subEnd,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &subID,
		CreatedAt:   fixedNow,
		UpdatedAt:   fixedNow,
	}
	suite.InsertEntitlement(context.Background(), existing)

	body, err := json.Marshal(map[string]any{
		"entitlement": "premium",
		"days":        7,
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/admin/users/"+userID+"/entitlements", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")

	suite.Server.Handler().ServeHTTP(w, req)
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
