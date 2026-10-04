package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/merchant"
)

type observedRequest struct{ method, path, slug, id, auth string }

func observe(r *http.Request) observedRequest {
	slug, id := selection(r)
	return observedRequest{r.Method, r.URL.Path, slug, id, r.Header.Get("Authorization")}
}

// selection is the request's one OpenRails-Merchant selector, by form.
func selection(r *http.Request) (slug, id string) {
	selector, _, err := merchant.ParseSelector(r.Header)
	if err != nil {
		return "invalid", "invalid"
	}
	if !selector.ID.IsZero() {
		id = selector.ID.String()
	}
	return selector.Slug, id
}

// targetCredential mints a credential naming the requested target, so the
// header proves which selector reached the credential provider.
func targetCredential(_ context.Context, target CredentialTarget) (string, error) {
	if (target.MerchantSlug == "") == target.MerchantID.IsZero() {
		return "", fmt.Errorf("target must name exactly one merchant: %+v", target)
	}
	if target.MerchantSlug != "" {
		return "slug:" + target.MerchantSlug, nil
	}
	return "id:" + target.MerchantID.String(), nil
}

func catalogApplication() *catalog.Application {
	return &catalog.Application{SchemaVersion: 1, ApplicationID: uuid.NewString(), ExpectedRevision: new(int64),
		Products: []catalog.ApplyProduct{{Key: "post", DisplayName: catalog.Value("Post")}}}
}

// Exactly one selector, a slug or an id, reaches both the OpenRails-Merchant
// header and the credential provider; the path never depends on it.
func TestMerchantSelectionRoutesOneTarget(t *testing.T) {
	seen := make(chan observedRequest, 1)
	id := billing.MerchantID(uuid.New())
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- observe(r)
		_, _ = w.Write([]byte(`{}`))
	}, WithDefaultMerchant(" Alpha "), WithCredentialProvider(targetCredential))
	cases := []struct {
		name, version, slug, id, auth string
		options                       []RequestOption
	}{
		{"client default", "/v1", "alpha", "", "Bearer slug:alpha", nil},
		{"slug override", "/v1", "bravo", "", "Bearer slug:bravo", []RequestOption{WithMerchant("BRAVO ")}},
		{"ID override", "/v1", "", id.String(), "Bearer id:" + id.String(), []RequestOption{ForMerchantID(id)}},
		{"UUID-shaped slug stays a slug", "/v1", id.String(), "", "Bearer slug:" + id.String(), []RequestOption{WithMerchant(id.String())}},
		{"nil options ignored", "/v1", "alpha", "", "Bearer slug:alpha", []RequestOption{nil}},
	}
	calls := map[string]func(opts []RequestOption) (string, error){
		"GET /merchant/configuration": func(o []RequestOption) (string, error) {
			return http.MethodGet, readConfiguration(client, t.Context(), o...)
		},
		"POST /merchant/catalog/applications": func(o []RequestOption) (string, error) {
			_, err := client.ApplyCatalog(t.Context(), catalogApplication(), o...)
			return http.MethodPost, err
		},
		"GET /merchant/catalog/revision": func(o []RequestOption) (string, error) {
			_, err := client.GetCatalogRevision(t.Context(), o...)
			return http.MethodGet, err
		},
		"GET /merchant/payments/" + billing.PaymentID(id).String(): func(o []RequestOption) (string, error) {
			_, err := client.GetPayment(t.Context(), billing.PaymentID(id), o...)
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
	require.NoError(t, readConfiguration(byID, t.Context()))
	require.Equal(t, observedRequest{http.MethodGet, "/v1/merchant/configuration", "", id.String(), "Bearer id:" + id.String()}, <-seen)
	require.NoError(t, readConfiguration(byID, t.Context(), WithMerchant("bravo")))
	require.Equal(t, observedRequest{http.MethodGet, "/v1/merchant/configuration", "bravo", "", "Bearer slug:bravo"}, <-seen)
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
		"zero ID":          {ForMerchantID(billing.MerchantID{})},
		"two slugs":        {WithMerchant("alpha"), WithMerchant("alpha")},
		"slug and ID":      {WithMerchant("alpha"), ForMerchantID(billing.MerchantID(uuid.New()))},
		"invalid then ok":  {WithMerchant("a b"), WithMerchant("alpha")},
		"only nil options": {nil, nil},
	} {
		err := readConfiguration(client, t.Context(), options...)
		require.ErrorIs(t, err, billing.ErrInvalid, name)
		var status *billing.StatusError
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
		err := readConfiguration(client, t.Context(), WithMerchant("restricted"))
		require.Error(t, err, name)
		if name == "error" {
			require.ErrorIs(t, err, failure)
		}
	}
}

// Ambient context may assert an ID but never select one; a slug cannot be
// checked against it before server resolution.
func TestAmbientMerchantAssertion(t *testing.T) {
	id, other := billing.MerchantID(uuid.New()), billing.MerchantID(uuid.New())
	var calls atomic.Int64
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "id:"+id.String(), r.Header.Get(merchant.SelectorHeader))
		_, _ = w.Write([]byte(`{}`))
	})
	require.ErrorIs(t, readConfiguration(client, merchant.WithID(t.Context(), other), ForMerchantID(id)), billing.ErrConflict)
	require.ErrorIs(t, readConfiguration(client, merchant.WithID(t.Context(), id)), billing.ErrInvalid, "slug default with an ambient ID")
	require.ErrorIs(t, readConfiguration(client, merchant.WithID(t.Context(), id), WithMerchant("alpha")), billing.ErrInvalid)
	require.Zero(t, calls.Load())
	require.NoError(t, readConfiguration(client, merchant.WithID(t.Context(), id), ForMerchantID(id)))
	require.NoError(t, readConfiguration(client, merchant.WithID(t.Context(), billing.MerchantID{}), ForMerchantID(id)), "a zero ambient ID asserts nothing")
	require.EqualValues(t, 2, calls.Load())
}

// Caller headers cannot add a second target, override credentials, or be mutated.
func TestExtraHeadersCannotDuplicateSelectionOrAuthority(t *testing.T) {
	id := billing.MerchantID(uuid.New())
	for _, byID := range []bool{false, true} {
		client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
			want := "alpha"
			if byID {
				want = "id:" + id.String()
			}
			require.Equal(t, []string{want}, r.Header.Values(merchant.SelectorHeader))
			require.Equal(t, []string{"Bearer selected-key"}, r.Header.Values("Authorization"))
			require.Equal(t, "kept", r.Header.Get("X-Trace"))
			_, _ = w.Write([]byte(`{}`))
		}, WithAPIKey("selected-key"))
		extra := http.Header{
			merchant.SelectorHeader: {"wrong", "another"}, strings.ToLower(merchant.SelectorHeader): {"lowercase"},
			"authorization": {"Bearer broader-key"}, "Authorization": {"Bearer broader-key"}, "X-Trace": {"kept"},
		}
		original := extra.Clone()
		option := WithMerchant("alpha")
		if byID {
			option = ForMerchantID(id)
		}
		require.NoError(t, client.doWithHeaders(t.Context(), http.MethodGet, "/v1/merchant/configuration", nil, nil, extra, option))
		require.True(t, reflect.DeepEqual(extra, original), "request mutated the caller's header map")
	}
}

func TestCatalogOwnerViewIsCatalogOnly(t *testing.T) {
	var calls atomic.Int64
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/v1/catalog/products", r.URL.Path)
		require.Equal(t, "Y2hhbm5lbC_DqQ", r.Header.Get("OpenRails-Catalog-Owner"), "owner is base64url of the UTF-8 subject")
		_, _ = w.Write([]byte(`{}`))
	})
	owner, err := client.ForCatalogOwner("channel/é")
	require.NoError(t, err)
	_, err = owner.CreateProduct(t.Context(), billing.CreateProductParams{Key: "post", DisplayName: "Post"})
	require.NoError(t, err)

	var denied *billing.StatusError
	require.ErrorAs(t, readConfiguration(owner, t.Context()), &denied)
	require.Equal(t, http.StatusForbidden, denied.Status)
	_, err = owner.ApplyCatalog(t.Context(), catalogApplication())
	require.ErrorIs(t, err, billing.ErrDenied)
	_, err = owner.GetCatalogRevision(t.Context())
	require.ErrorIs(t, err, billing.ErrDenied)
	_, err = owner.ForCatalogOwner("someone-else")
	require.ErrorIs(t, err, billing.ErrDenied)
	for _, subject := range []string{"", "a\x00b", "\xff"} {
		_, err := client.ForCatalogOwner(subject)
		require.ErrorIs(t, err, billing.ErrInvalid)
	}
	require.EqualValues(t, 1, calls.Load())
}

// Per-operation options never mutate the shared Client under concurrency.
func TestConcurrentSelectionDoesNotContaminate(t *testing.T) {
	var requests atomic.Int64
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		slug, id := selection(r)
		if r.Header.Get("Authorization") != "Bearer slug:"+slug || id != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": billing.ProductID(uuid.New()).String(), "display_name": slug})
	}, WithDefaultMerchant("alpha"), WithCredentialProvider(targetCredential))
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			slug, options := "alpha", []RequestOption(nil)
			if i%2 == 1 {
				slug, options = "bravo", []RequestOption{WithMerchant("bravo")}
			}
			product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: fmt.Sprintf("post-%d", i), DisplayName: "post"}, options...)
			if err != nil || product.DisplayName != slug {
				t.Errorf("operation for %s returned %+v, %v", slug, product, err)
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 64, requests.Load())
	require.Equal(t, "alpha", client.merchantSlug)
}

// readConfiguration is one authenticated merchant read.
func readConfiguration(c *Client, ctx context.Context, options ...RequestOption) error {
	_, err := c.GetMerchantConfiguration(ctx, options...)
	return err
}
