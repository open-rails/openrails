package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/merchant"
)

type observedRequest struct{ method, path, slug, id, auth string }

func observe(r *http.Request) observedRequest {
	return observedRequest{r.Method, r.URL.Path, r.Header.Get(merchant.SlugHeader), r.Header.Get(merchant.BindingHeader), r.Header.Get("Authorization")}
}

// targetCredential mints a credential naming the requested target, so the
// header proves which selector reached the credential provider.
func targetCredential(_ context.Context, target CredentialTarget) (string, error) {
	if target.Scope != CredentialScopeMerchant || (target.MerchantSlug == "") == target.MerchantID.IsZero() {
		return "", fmt.Errorf("target must name exactly one merchant: %+v", target)
	}
	if target.MerchantSlug != "" {
		return "slug:" + target.MerchantSlug, nil
	}
	return "id:" + target.MerchantID.String(), nil
}

func catalogApplication() *CatalogApplyParams {
	return &CatalogApplyParams{SchemaVersion: 1, ApplicationID: uuid.NewString(), ExpectedRevision: new(int64),
		Products: []CatalogApplyProduct{{Key: "post", DisplayName: CatalogValue("Post")}}}
}

// Slug selection uses /v2 (a pre-selector server cannot execute it under the
// credential's merchant); ID selection keeps the /v1 assertion protocol.
// Exactly one selector reaches both the headers and the credential provider.
func TestMerchantSelectionRoutesOneTarget(t *testing.T) {
	seen := make(chan observedRequest, 1)
	id := MerchantID(uuid.New())
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- observe(r)
		_, _ = w.Write([]byte(`{}`))
	}, WithDefaultMerchant(" Alpha "), WithCredentialProvider(targetCredential))
	cases := []struct {
		name, version, slug, id, auth string
		options                       []RequestOption
	}{
		{"client default", "/v2", "alpha", "", "Bearer slug:alpha", nil},
		{"slug override", "/v2", "bravo", "", "Bearer slug:bravo", []RequestOption{WithMerchant("BRAVO ")}},
		{"ID override", "/v1", "", id.String(), "Bearer id:" + id.String(), []RequestOption{ForMerchantID(id)}},
		{"UUID-shaped slug stays a slug", "/v2", id.String(), "", "Bearer slug:" + id.String(), []RequestOption{WithMerchant(id.String())}},
		{"nil options ignored", "/v2", "alpha", "", "Bearer slug:alpha", []RequestOption{nil}},
	}
	calls := map[string]func(opts []RequestOption) (string, error){
		"GET /merchant/settings": func(o []RequestOption) (string, error) { return http.MethodGet, client.Verify(t.Context(), o...) },
		"POST /merchant/catalog/applications": func(o []RequestOption) (string, error) {
			_, err := client.Catalog.Apply(t.Context(), catalogApplication(), o...)
			return http.MethodPost, err
		},
		"GET /merchant/catalog/revision": func(o []RequestOption) (string, error) {
			_, err := client.Catalog.Revision(t.Context(), o...)
			return http.MethodGet, err
		},
		"GET /merchant/payments/" + PaymentID(id).String(): func(o []RequestOption) (string, error) {
			_, err := client.GetPayment(t.Context(), PaymentID(id), o...)
			return http.MethodGet, err
		},
	}
	for _, tc := range cases {
		for route, call := range calls {
			method, err := call(tc.options)
			require.NoError(t, err, tc.name+" "+route)
			path := strings.TrimPrefix(route, method+" ")
			require.Equal(t, observedRequest{method, tc.version + path, tc.slug, tc.id, tc.auth}, <-seen, tc.name+" "+route)
		}
	}
	require.True(t, client.MerchantID().IsZero())

	byID := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- observe(r)
		_, _ = w.Write([]byte(`{}`))
	}, WithMerchantID(id), WithCredentialProvider(targetCredential))
	require.Equal(t, id, byID.MerchantID())
	require.NoError(t, byID.Verify(t.Context()))
	require.Equal(t, observedRequest{http.MethodGet, "/v1/merchant/settings", "", id.String(), "Bearer id:" + id.String()}, <-seen)
	require.NoError(t, byID.Verify(t.Context(), WithMerchant("bravo")))
	require.Equal(t, observedRequest{http.MethodGet, "/v2/merchant/settings", "bravo", "", "Bearer slug:bravo"}, <-seen)
}

func TestInvalidMerchantSelectionFailsBeforeCredentialMint(t *testing.T) {
	var minted atomic.Int64
	client, err := NewRemote("https://never-called.invalid", WithCredentialProvider(func(context.Context, CredentialTarget) (string, error) {
		minted.Add(1)
		return "must-not-mint", nil
	}))
	require.NoError(t, err)
	for name, options := range map[string][]RequestOption{
		"missing":          nil,
		"empty slug":       {WithMerchant("  ")},
		"bad slug":         {WithMerchant("alpha/bravo")},
		"leading hyphen":   {WithMerchant("-alpha")},
		"zero ID":          {ForMerchantID(MerchantID{})},
		"two slugs":        {WithMerchant("alpha"), WithMerchant("alpha")},
		"slug and ID":      {WithMerchant("alpha"), ForMerchantID(MerchantID(uuid.New()))},
		"invalid then ok":  {WithMerchant("a b"), WithMerchant("alpha")},
		"only nil options": {nil, nil},
	} {
		err := client.Verify(t.Context(), options...)
		require.ErrorIs(t, err, ErrInvalid, name)
		var status *StatusError
		require.ErrorAs(t, err, &status, name)
		require.Equal(t, "invalid_param", status.Code, name)
	}
	require.Zero(t, minted.Load())
}

func TestCredentialFailureNeverFallsBack(t *testing.T) {
	failure := errors.New("no credential for this merchant")
	for name, provider := range map[string]func(context.Context, CredentialTarget) (string, error){
		"error": func(context.Context, CredentialTarget) (string, error) { return "", failure },
		"blank": func(context.Context, CredentialTarget) (string, error) { return " ", nil },
	} {
		client := newTestRemote(t, func(http.ResponseWriter, *http.Request) { t.Error("request sent without a minted credential") },
			WithAPIKey("broader-host-key"), WithCredentialProvider(provider))
		err := client.Verify(t.Context(), WithMerchant("restricted"))
		require.Error(t, err, name)
		if name == "error" {
			require.ErrorIs(t, err, failure)
		}
	}
}

// Ambient context may assert an ID but never select one; a slug cannot be
// checked against it before server resolution.
func TestAmbientMerchantAssertion(t *testing.T) {
	id, other := MerchantID(uuid.New()), MerchantID(uuid.New())
	var calls atomic.Int64
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, id.String(), r.Header.Get(merchant.BindingHeader))
		_, _ = w.Write([]byte(`{}`))
	})
	require.ErrorIs(t, client.Verify(merchant.WithID(t.Context(), other), ForMerchantID(id)), ErrConflict)
	require.ErrorIs(t, client.Verify(merchant.WithID(t.Context(), id)), ErrInvalid, "slug default with an ambient ID")
	require.ErrorIs(t, client.Verify(merchant.WithID(t.Context(), id), WithMerchant("alpha")), ErrInvalid)
	require.Zero(t, calls.Load())
	require.NoError(t, client.Verify(merchant.WithID(t.Context(), id), ForMerchantID(id)))
	require.NoError(t, client.Verify(merchant.WithID(t.Context(), MerchantID{}), ForMerchantID(id)), "a zero ambient ID asserts nothing")
	require.EqualValues(t, 2, calls.Load())
}

// Caller headers cannot add a second target, override credentials, or be mutated.
func TestExtraHeadersCannotDuplicateSelectionOrAuthority(t *testing.T) {
	id := MerchantID(uuid.New())
	for _, byID := range []bool{false, true} {
		client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
			slugs, ids := r.Header.Values(merchant.SlugHeader), r.Header.Values(merchant.BindingHeader)
			if byID {
				require.Empty(t, slugs)
				require.Equal(t, []string{id.String()}, ids)
			} else {
				require.Empty(t, ids)
				require.Equal(t, []string{"alpha"}, slugs)
			}
			require.Equal(t, []string{"Bearer selected-key"}, r.Header.Values("Authorization"))
			require.Equal(t, "kept", r.Header.Get("X-Trace"))
			_, _ = w.Write([]byte(`{}`))
		}, WithAPIKey("selected-key"))
		extra := http.Header{
			merchant.BindingHeader: {"wrong", "another"}, strings.ToLower(merchant.BindingHeader): {"lowercase"},
			merchant.SlugHeader: {"wrong"}, strings.ToLower(merchant.SlugHeader): {"lowercase"},
			"authorization": {"Bearer broader-key"}, "Authorization": {"Bearer broader-key"}, "X-Trace": {"kept"},
		}
		original := extra.Clone()
		option := WithMerchant("alpha")
		if byID {
			option = ForMerchantID(id)
		}
		require.NoError(t, client.doWithHeaders(t.Context(), http.MethodGet, "/v1/merchant/settings", nil, nil, extra, option))
		require.True(t, reflect.DeepEqual(extra, original), "request mutated the caller's header map")
	}
}

// A pre-selector server ignores the slug header and would write under the
// credential's merchant. Slug selection must use a route it does not serve.
func TestSlugSelectionCannotWriteThroughOldServer(t *testing.T) {
	var writes atomic.Int64
	mux := http.NewServeMux()
	for _, path := range []string{"/v1/merchant/catalog/products", "/v1/merchant/catalog/applications"} {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, _ *http.Request) {
			writes.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{})
		})
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	id := MerchantID(uuid.New())
	client, err := NewRemote(server.URL, WithAPIKey("merchant-alpha-key"), WithMerchantID(id))
	require.NoError(t, err)
	_, err = client.Products.Create(t.Context(), &ProductCreateParams{Key: "bravo-post", DisplayName: "Bravo"}, WithMerchant("bravo"))
	require.ErrorIs(t, err, ErrNotFound)
	_, err = client.Catalog.Apply(t.Context(), catalogApplication(), WithMerchant("bravo"))
	require.ErrorIs(t, err, ErrNotFound)
	require.Zero(t, writes.Load())
	_, err = client.Catalog.Apply(t.Context(), catalogApplication(), ForMerchantID(id))
	require.NoError(t, err, "positive control: the old route accepts this credential")
	require.EqualValues(t, 1, writes.Load())
}

func TestCatalogOwnerViewIsCatalogOnly(t *testing.T) {
	var calls atomic.Int64
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/v2/catalog/products", r.URL.Path)
		require.Equal(t, "Y2hhbm5lbC_DqQ", r.Header.Get("OpenRails-Catalog-Owner"), "owner is base64url of the UTF-8 subject")
		_, _ = w.Write([]byte(`{}`))
	})
	owner, err := client.ForCatalogOwner("channel/é")
	require.NoError(t, err)
	require.NotSame(t, client.Catalog, owner.Catalog, "copied views rebind resources to their own parent")
	_, err = owner.Products.Create(t.Context(), &ProductCreateParams{Key: "post", DisplayName: "Post"})
	require.NoError(t, err)

	var denied *StatusError
	require.ErrorAs(t, owner.Verify(t.Context()), &denied)
	require.Equal(t, http.StatusForbidden, denied.Status)
	_, err = owner.Catalog.Apply(t.Context(), catalogApplication())
	require.Error(t, err)
	_, err = owner.Catalog.Revision(t.Context())
	require.Error(t, err)
	_, err = owner.ForCatalogOwner("someone-else")
	require.ErrorIs(t, err, ErrDenied)
	for _, subject := range []string{"", "a\x00b", "\xff"} {
		_, err := client.ForCatalogOwner(subject)
		require.ErrorIs(t, err, ErrInvalid)
	}
	require.EqualValues(t, 1, calls.Load())
}

// Per-operation options never mutate the shared Client under concurrency.
func TestConcurrentSelectionDoesNotContaminate(t *testing.T) {
	var requests atomic.Int64
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		slug := r.Header.Get(merchant.SlugHeader)
		if r.Header.Get("Authorization") != "Bearer slug:"+slug || r.Header.Get(merchant.BindingHeader) != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": ProductID(uuid.New()).String(), "display_name": slug})
	}, WithDefaultMerchant("alpha"), WithCredentialProvider(targetCredential))
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			slug, options := "alpha", []RequestOption(nil)
			if i%2 == 1 {
				slug, options = "bravo", []RequestOption{WithMerchant("bravo")}
			}
			product, err := client.Products.Create(t.Context(), &ProductCreateParams{Key: fmt.Sprintf("post-%d", i), DisplayName: "post"}, options...)
			if err != nil || product.DisplayName != slug {
				t.Errorf("operation for %s returned %+v, %v", slug, product, err)
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 64, requests.Load())
	require.Equal(t, "alpha", client.merchantSlug)
}
