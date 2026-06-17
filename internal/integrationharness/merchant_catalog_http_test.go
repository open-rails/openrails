//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestStandaloneMerchantCatalogRoutesHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	catalogToken := surface.MintServiceToken(
		dbtest.TestMerchantSlug,
		"catalog-writer-"+uuid.NewString(),
		[]string{controlplane.PermCatalogWrite},
		[]authcore.ServiceTokenResource{controlplane.MerchantResource(dbtest.TestMerchantID)},
	)
	readOnlyToken := surface.MintServiceToken(
		dbtest.TestMerchantSlug,
		"catalog-denied-"+uuid.NewString(),
		[]string{controlplane.PermCreditsRead},
		[]authcore.ServiceTokenResource{controlplane.MerchantResource(dbtest.TestMerchantID)},
	)

	productSlug := "catalog-route-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	createStatus, createBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/products", catalogToken, map[string]any{
		"slug":         productSlug,
		"display_name": "Catalog Route Product",
		"description":  "created through the live merchant catalog route",
	})
	require.Equal(t, http.StatusCreated, createStatus, string(createBody))
	var created struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	require.NoError(t, json.Unmarshal(createBody, &created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, productSlug, created.Slug)

	getStatus, getBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/products/by-slug/"+productSlug, catalogToken, nil)
	require.Equal(t, http.StatusOK, getStatus, string(getBody))
	var fetched struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	require.NoError(t, json.Unmarshal(getBody, &fetched))
	require.Equal(t, created.ID, fetched.ID)
	require.Equal(t, productSlug, fetched.Slug)

	oldStatus, oldBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/admin/catalog/products", catalogToken, nil)
	require.Equal(t, http.StatusNotFound, oldStatus, string(oldBody))

	unauthStatus, unauthBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/products", "", nil)
	require.Equal(t, http.StatusUnauthorized, unauthStatus, string(unauthBody))

	deniedStatus, deniedBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/products", readOnlyToken, nil)
	require.Equal(t, http.StatusForbidden, deniedStatus, string(deniedBody))
}

func requestJSON(t *testing.T, method, url, token string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, &buf)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}
