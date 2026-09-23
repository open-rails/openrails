package openrails

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/merchant"
)

func catalogSelectorApplication() *CatalogApplyParams {
	return &CatalogApplyParams{SchemaVersion: 1, ApplicationID: uuid.NewString(), ExpectedRevision: new(int64),
		Products: []CatalogApplyProduct{{Key: "post", DisplayName: CatalogValue("Post")}}}
}

func TestCatalogApplicationRequestSelection(t *testing.T) {
	type observation struct{ method, path, slug, id, credential string }
	seen := make(chan observation, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- observation{r.Method, r.URL.Path, r.Header.Get(merchant.SlugHeader), r.Header.Get(merchant.BindingHeader), r.Header.Get("Authorization")}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	client, err := NewRemote(server.URL, WithDefaultMerchant("alpha"), WithCredentialProvider(func(_ context.Context, target CredentialTarget) (string, error) {
		require.Equal(t, CredentialScopeMerchant, target.Scope)
		if target.MerchantSlug != "" {
			return "slug:" + target.MerchantSlug, nil
		}
		return "id:" + target.MerchantID.String(), nil
	}))
	require.NoError(t, err)
	id := MerchantID(uuid.New())
	for _, selector := range []struct {
		name, slug, id, version, credential string
		options                             []RequestOption
	}{
		{"slug override", "bravo", "", "/v2", "Bearer slug:bravo", []RequestOption{WithMerchant("bravo")}},
		{"ID override", "", id.String(), "/v1", "Bearer id:" + id.String(), []RequestOption{ForMerchantID(id)}},
		{"unchanged default", "alpha", "", "/v2", "Bearer slug:alpha", nil},
	} {
		t.Run(selector.name, func(t *testing.T) {
			_, err := client.Catalog.Apply(t.Context(), catalogSelectorApplication(), selector.options...)
			require.NoError(t, err)
			require.Equal(t, observation{http.MethodPost, selector.version + "/merchant/catalog/applications", selector.slug, selector.id, selector.credential}, <-seen)
			_, err = client.Catalog.Revision(t.Context(), selector.options...)
			require.NoError(t, err)
			require.Equal(t, observation{http.MethodGet, selector.version + "/merchant/catalog/revision", selector.slug, selector.id, selector.credential}, <-seen)
		})
	}

	owner, err := client.ForCatalogOwner("channel")
	require.NoError(t, err)
	require.NotSame(t, client.Catalog, owner.Catalog, "copied views must rebind the new resource to their own parent")
	_, err = owner.Catalog.Apply(t.Context(), catalogSelectorApplication(), WithMerchant("bravo"))
	require.ErrorContains(t, err, "merchant catalog authority")
	_, err = owner.Catalog.Revision(t.Context(), ForMerchantID(id))
	require.ErrorContains(t, err, "merchant catalog authority")
	require.Empty(t, seen, "catalog-owner views cannot reach merchant application/revision routes")

	unselected, err := NewRemote(server.URL, WithAPIKey("key"))
	require.NoError(t, err)
	_, err = unselected.Catalog.Apply(t.Context(), catalogSelectorApplication())
	require.ErrorIs(t, err, ErrInvalid)
	_, err = unselected.Catalog.Revision(t.Context())
	require.ErrorIs(t, err, ErrInvalid)
	require.Empty(t, seen, "these merchant operations must fail before dispatch without selection")
}

func TestCatalogApplicationSlugCannotWriteThroughOldServer(t *testing.T) {
	var writes atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/merchant/catalog/applications", func(w http.ResponseWriter, r *http.Request) {
		writes.Add(1)
		_ = json.NewEncoder(w).Encode(CatalogApplicationReceipt{AppliedRevision: 1})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	id := MerchantID(uuid.New())
	client, err := NewRemote(server.URL, WithAPIKey("alpha-key"), WithMerchantID(id))
	require.NoError(t, err)
	_, err = client.Catalog.Apply(t.Context(), catalogSelectorApplication(), WithMerchant("bravo"))
	require.ErrorIs(t, err, ErrNotFound)
	require.Zero(t, writes.Load(), "slug selection must not fall back to the credential's v1 write route")
	_, err = client.Catalog.Apply(t.Context(), catalogSelectorApplication(), ForMerchantID(id))
	require.NoError(t, err, "positive control: explicit IDs still use the existing v1 assertion protocol")
	require.EqualValues(t, 1, writes.Load())
}
