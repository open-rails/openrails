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
