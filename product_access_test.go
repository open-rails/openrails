package openrails

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestProductAccessStringResourceRequests(t *testing.T) {
	customer := uuid.NewString()
	product := ProductID(uuid.New()).String()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "Bearer test", r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodPost:
			require.Equal(t, "/v1/merchant/users/"+customer+"/product-access/check", r.URL.Path)
			var body struct {
				ProductIDs []string `json:"product_ids"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, []string{product, product}, body.ProductIDs)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"access": map[string]bool{product: true}}))
		case r.URL.Query().Get("product_id") != "":
			require.Equal(t, product, r.URL.Query().Get("product_id"))
			require.NoError(t, json.NewEncoder(w).Encode(ProductAccessCheck{CustomerID: customer, ProductID: product, HasAccess: true}))
		default:
			require.Equal(t, "7", r.URL.Query().Get("limit"))
			require.Equal(t, "cursor", r.URL.Query().Get("cursor"))
			require.NoError(t, json.NewEncoder(w).Encode(ProductAccessList{Data: []ProductAccessGrant{{ProductID: product}}, HasMore: true, NextCursor: "next"}))
		}
	}))
	defer server.Close()
	client, err := NewRemote(server.URL, WithAPIKey("test"))
	require.NoError(t, err)
	single, err := client.ProductAccess.Check(t.Context(), &ProductAccessCheckParams{CustomerID: customer, ProductID: product})
	require.NoError(t, err)
	require.True(t, single.HasAccess)
	batch, err := client.ProductAccess.CheckMany(t.Context(), &ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: []string{product, product}})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{product: true}, batch)
	page, err := client.ProductAccess.List(t.Context(), &ProductAccessListParams{CustomerID: customer, Limit: 7, Cursor: "cursor"})
	require.NoError(t, err)
	require.Equal(t, "next", page.NextCursor)
	require.True(t, page.HasMore)
	require.Equal(t, 3, calls)
	empty, err := client.ProductAccess.CheckMany(t.Context(), &ProductAccessCheckManyParams{CustomerID: customer})
	require.NoError(t, err)
	require.Empty(t, empty)
	for _, ids := range [][]string{{"not-a-product"}, {PriceID(uuid.New()).String()}, strings.Split(strings.Repeat(product+",", 101), ",")} {
		_, err = client.ProductAccess.CheckMany(t.Context(), &ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: ids})
		require.ErrorIs(t, err, ErrInvalid)
	}
	_, err = client.ProductAccess.List(t.Context(), &ProductAccessListParams{CustomerID: customer, Limit: 101})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = client.ProductAccess.Check(t.Context(), &ProductAccessCheckParams{CustomerID: "not-a-customer", ProductID: product})
	require.ErrorIs(t, err, ErrInvalid)
	require.Equal(t, 3, calls, "invalid input never reaches transport")
}
