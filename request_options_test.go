package openrails

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestClientConcurrentMerchantSelection(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := r.Header.Get(merchant.SlugHeader)
		if (slug != "alpha" && slug != "bravo") || r.Header.Get(merchant.BindingHeader) != "" || r.Header.Get("Authorization") != "Bearer credential-"+slug {
			t.Errorf("target/credential contamination: %v", r.Header)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": uuid.NewString(), "display_name": slug})
	}))
	t.Cleanup(server.Close)
	client, err := NewRemote(server.URL, WithDefaultMerchant("alpha"), WithCredentialProvider(func(_ context.Context, target CredentialTarget) (string, error) {
		if target.Scope != CredentialScopeMerchant || !target.MerchantID.IsZero() {
			return "", fmt.Errorf("unexpected credential target: %+v", target)
		}
		return "credential-" + target.MerchantSlug, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			slug := "alpha"
			var options []RequestOption
			if i%2 != 0 {
				slug = "bravo"
				options = []RequestOption{WithMerchant(slug)}
			}
			product, err := client.Products.Create(t.Context(), &ProductCreateParams{Key: fmt.Sprintf("post-%d", i), DisplayName: "post"}, options...)
			if err != nil || product == nil || product.DisplayName != slug {
				t.Errorf("operation for %s returned %+v, %v", slug, product, err)
			}
		}(i)
	}
	wg.Wait()
	if requests.Load() != 64 || client.merchantSlug != "alpha" || !client.MerchantID().IsZero() {
		t.Fatalf("shared Client changed or calls lost: requests=%d, slug=%q, id=%s", requests.Load(), client.merchantSlug, client.MerchantID())
	}
}

func TestClientMerchantSelectionFailsBeforeCredentialMint(t *testing.T) {
	var minted atomic.Int64
	client, err := NewRemote("https://never-called.invalid", WithCredentialProvider(func(context.Context, CredentialTarget) (string, error) {
		minted.Add(1)
		return "must-not-mint", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	for name, options := range map[string][]RequestOption{
		"missing":     nil,
		"empty":       {WithMerchant("")},
		"bad slug":    {WithMerchant("alpha/bravo")},
		"zero ID":     {ForMerchantID(MerchantID{})},
		"two slugs":   {WithMerchant("alpha"), WithMerchant("alpha")},
		"slug and ID": {WithMerchant("alpha"), ForMerchantID(MerchantID(uuid.New()))},
	} {
		t.Run(name, func(t *testing.T) {
			if err := client.Verify(t.Context(), options...); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want invalid request", err)
			}
		})
	}
	if minted.Load() != 0 {
		t.Fatalf("invalid merchant requests minted %d credentials", minted.Load())
	}
	if _, err := NewRemote("https://never-called.invalid", WithAPIKey("key"), WithDefaultMerchant("")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty default accepted: %v", err)
	}
}

func TestClientCredentialFailureNeverFallsBack(t *testing.T) {
	failure := errors.New("no credential for this merchant")
	client, err := NewRemote("https://never-called.invalid", WithAPIKey("broader-host-key"), WithCredentialProvider(func(_ context.Context, target CredentialTarget) (string, error) {
		if target.MerchantSlug != "restricted" {
			t.Errorf("target lost: %+v", target)
		}
		return "", failure
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Verify(t.Context(), WithMerchant("restricted")); !errors.Is(err, failure) {
		t.Fatalf("credential failure lost or fell back: %v", err)
	}
}

func TestClientRequestSelectionReachesResourceAndScopedOperations(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get(merchant.SlugHeader) != "selected" || r.Header.Get(merchant.BindingHeader) != "" {
			t.Errorf("request options not forwarded by %s %s: %v", r.Method, r.URL.Path, r.Header)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	client, err := NewRemote(server.URL, WithAPIKey("test"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := client.ForCatalogOwner("channel-owner")
	if err != nil {
		t.Fatal(err)
	}
	product, price, customer := ProductID(uuid.New()).String(), PriceID(uuid.New()).String(), CustomerID(uuid.New()).String()
	selected := WithMerchant("selected")
	for name, call := range map[string]func() error{
		"product read": func() error { _, err := client.Products.Retrieve(t.Context(), product, selected); return err },
		"price read":   func() error { _, err := client.Prices.Retrieve(t.Context(), price, selected); return err },
		"price update": func() error {
			_, err := client.Prices.Update(t.Context(), price, &PriceUpdateParams{}, selected)
			return err
		},
		"catalog owner create": func() error {
			_, err := owner.Products.Create(t.Context(), &ProductCreateParams{Key: "post", DisplayName: "Post"}, selected)
			return err
		},
		"bounded access check": func() error {
			_, err := client.ProductAccess.CheckMany(t.Context(), &ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: []string{product}}, selected)
			return err
		},
		"subscription cancellation": func() error {
			return client.CancelSubscription(t.Context(), SubscriptionID(uuid.New()), CancelSubscriptionRequest{}, selected)
		},
		"payment read":          func() error { _, err := client.GetPayment(t.Context(), PaymentID(uuid.New()), selected); return err },
		"settings verification": func() error { return client.Verify(t.Context(), selected) },
	} {
		t.Run(name, func(t *testing.T) {
			before := calls.Load()
			if err := call(); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != before+1 {
				t.Fatal("operation did not reach the HTTP boundary exactly once")
			}
		})
	}
	before := calls.Load()
	var refusal *StatusError
	if err := owner.Verify(t.Context(), selected); !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden {
		t.Fatalf("merchant option escaped catalog-only client: %v", err)
	}
	if calls.Load() != before {
		t.Fatal("catalog-only client issued a merchant-admin request")
	}
}

func TestClientMerchantIDAndStreamingSelection(t *testing.T) {
	id := MerchantID(uuid.New())
	archive := archiveTransportFixture(t, id, 1)
	var called int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Header.Get(merchant.BindingHeader) != id.String() || r.Header.Get(merchant.SlugHeader) != "" || r.Header.Get("Authorization") != "Bearer "+id.String() {
			t.Errorf("ID stream selection lost: %v", r.Header)
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(archive)
			return
		}
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, archive) {
			t.Errorf("archive changed: %v", err)
		}
		_ = json.NewEncoder(w).Encode(MerchantBillingImportResult{MerchantID: id, Rows: 1})
	}))
	t.Cleanup(server.Close)
	client, err := NewRemote(server.URL, WithDefaultMerchant("overridden"), WithCredentialProvider(func(_ context.Context, target CredentialTarget) (string, error) {
		if target.Scope != CredentialScopeMerchant || target.MerchantSlug != "" || target.MerchantID != id {
			t.Errorf("wrong stream credential target: %+v", target)
		}
		return target.MerchantID.String(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var downloaded bytes.Buffer
	if err := client.ExportMerchantBilling(t.Context(), &downloaded, ForMerchantID(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ImportMerchantBilling(t.Context(), bytes.NewReader(downloaded.Bytes()), ForMerchantID(id)); err != nil {
		t.Fatal(err)
	}
	if called != 2 || client.merchantSlug != "overridden" {
		t.Fatalf("stream calls/default changed: %d, %q", called, client.merchantSlug)
	}
	ctx := merchant.WithID(t.Context(), MerchantID(uuid.New()))
	if err := client.Verify(ctx, ForMerchantID(id)); !errors.Is(err, ErrConflict) {
		t.Fatalf("ambient ID mismatch allowed: %v", err)
	}
	if err := client.Verify(ctx, WithMerchant("alpha")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ambiguous slug/ambient ID allowed: %v", err)
	}
}

func TestClientExtraHeadersCannotDuplicateMerchantSelection(t *testing.T) {
	id := MerchantID(uuid.New())
	for _, byID := range []bool{false, true} {
		t.Run(fmt.Sprint(byID), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				slugs, ids := r.Header.Values(merchant.SlugHeader), r.Header.Values(merchant.BindingHeader)
				if byID {
					if len(slugs) != 0 || !reflect.DeepEqual(ids, []string{id.String()}) {
						t.Errorf("duplicate/wrong targets: slugs=%v ids=%v", slugs, ids)
					}
				} else if len(ids) != 0 || !reflect.DeepEqual(slugs, []string{"alpha"}) {
					t.Errorf("duplicate/wrong targets: slugs=%v ids=%v", slugs, ids)
				}
				if !reflect.DeepEqual(r.Header.Values("Authorization"), []string{"Bearer selected-key"}) {
					t.Errorf("extra headers supplied alternate authority: %v", r.Header.Values("Authorization"))
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(server.Close)
			client, err := NewRemote(server.URL, WithAPIKey("selected-key"))
			if err != nil {
				t.Fatal(err)
			}
			extra := http.Header{
				merchant.BindingHeader: {"wrong", "another"}, strings.ToLower(merchant.BindingHeader): {"lowercase"},
				merchant.SlugHeader: {"wrong"}, strings.ToLower(merchant.SlugHeader): {"lowercase"},
				"authorization": {"Bearer broader-key"},
			}
			option := WithMerchant("alpha")
			if byID {
				option = ForMerchantID(id)
			}
			if err := client.doWithHeaders(t.Context(), http.MethodGet, "/v1/merchant/settings", nil, nil, extra, option); err != nil {
				t.Fatal(err)
			}
			if extra["authorization"][0] != "Bearer broader-key" {
				t.Fatal("request mutated the caller's header map")
			}
		})
	}
}
