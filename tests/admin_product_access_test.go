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
)

// TestAdminProductAccessGrantAndRevoke exercises the #528 delegated product-access
// WRITE surface end-to-end against Postgres: a merchant admin holding
// org:product_access:update grants a user ownership of a product
// (POST /v1/admin/users/:id/product-access), the grant appears in the composite
// user-detail product_access section, and a DELETE revokes it. Product access
// (#250) is a SEPARATE concept from entitlements, with its own write capability.
func TestAdminProductAccessGrantAndRevoke(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "be000000-0000-4000-8000-000000000005",
		[]string{controlplane.PermMerchantProductAccessWrite, controlplane.PermMerchantBillingRead})

	products := suite.SeedProducts()
	productID := products[0].Product.ID
	userID := uuid.New().String()

	// Grant ownership.
	body, _ := json.Marshal(map[string]any{"product_id": productID.String()})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/admin/users/"+userID+"/product-access", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer host-credential")
	req.Header.Set("Content-Type", "application/json")
	admin.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var grant struct {
		ID         string `json:"id"`
		CustomerID string `json:"customer_id"`
		ProductID  string `json:"product_id"`
		Status     string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &grant))
	require.Equal(t, userID, grant.CustomerID, "grant must land on the path user (#364 UUID subject)")
	require.Equal(t, productID.String(), grant.ProductID)
	require.Equal(t, "active", grant.Status)
	require.NotEmpty(t, grant.ID)

	// It appears in the composite user-detail product_access section.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/admin/users/"+userID, nil)
	req.Header.Set("Authorization", "Bearer host-credential")
	admin.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var profile map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &profile))
	pa, ok := profile["product_access"].([]any)
	require.True(t, ok, "composite must embed a product_access section: %s", w.Body.String())
	require.Len(t, pa, 1, "the granted product access should appear in the composite")

	// Revoke it.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/v1/admin/users/"+userID+"/product-access/"+grant.ID, nil)
	req.Header.Set("Authorization", "Bearer host-credential")
	admin.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestAdminProductAccess_RequiresProductAccessWrite proves the delegated gate
// fails closed: a principal holding only billing:read cannot grant product access.
func TestAdminProductAccess_RequiresProductAccessWrite(t *testing.T) {
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "be000000-0000-4000-8000-000000000006",
		[]string{controlplane.PermMerchantBillingRead})

	products := suite.SeedProducts()
	body, _ := json.Marshal(map[string]any{"product_id": products[0].Product.ID.String()})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/admin/users/"+uuid.New().String()+"/product-access", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer host-credential")
	req.Header.Set("Content-Type", "application/json")
	admin.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
