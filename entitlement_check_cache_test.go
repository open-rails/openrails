package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

func TestEntitlementCheckProcessCache(t *testing.T) {
	mid := billing.MerchantID(uuid.New())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != entitlementCheckPath || r.Method != http.MethodPost {
			t.Errorf("unexpected entitlement check: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing application credential")
		}
		var params billing.CheckEntitlementsParams
		if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if params.PrefixLimit < 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set(merchant.BoundHeader, mid.String())
		answer := billing.EntitlementCheck{
			Entitlements: map[string]bool{"premium": true, "rental": false},
			Quantities:   map[string]*int{"premium": new(3), "rental": nil},
		}
		if r.Header.Get(entitlementCacheMetadataHeader) == "1" {
			now := time.Now().UTC().Add(-10 * time.Minute)
			boundary := now.Add(10 * time.Second)
			answer.EvaluatedAt, answer.ValidUntil = &now, &boundary
		}
		if err := json.NewEncoder(w).Encode(answer); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client, err := NewRemoteMerchant(server.URL, RemoteCredential{APIKey: "test-key"}, RemoteMerchantOptions{
		EntitlementCheckCache: EntitlementCheckCacheOptions{MaxAge: 30 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	params := billing.CheckEntitlementsParams{CustomerID: billing.CustomerID(uuid.New()), Entitlements: []string{"rental", "premium"}}
	first, err := client.CheckEntitlements(t.Context(), params)
	if err != nil || first.EvaluatedAt == nil || !first.Entitlements["premium"] || first.Entitlements["rental"] {
		t.Fatalf("first check = %+v, %v", first, err)
	}
	if client.MerchantID() != mid {
		t.Fatal("handle did not adopt the merchant from the first response")
	}
	// Mutating a caller's response cannot change the cache. Reordering or
	// repeating exact keys doesn't change the entitlement check's result.
	first.Entitlements["premium"] = false
	*first.Quantities["premium"] = 99
	params.Entitlements = []string{"premium", "rental", "premium"}
	second, err := client.CheckEntitlements(t.Context(), params)
	if err != nil || !second.Entitlements["premium"] || second.Entitlements["rental"] || *second.Quantities["premium"] != 3 {
		t.Fatalf("second check = %+v, %v", second, err)
	}
	if count := requests.Load(); count != 1 {
		t.Fatalf("requests after repeated check = %d, want 1", count)
	}

	live, err := client.CheckEntitlementsLive(t.Context(), params)
	if err != nil || live.EvaluatedAt != nil {
		t.Fatalf("live check = %+v, %v", live, err)
	}
	historical := params
	historical.At = time.Now()
	if _, err := client.CheckEntitlements(t.Context(), historical); err != nil {
		t.Fatal(err)
	}
	prefix := params
	prefix.Prefixes = []string{"prefix:"}
	if _, err := client.CheckEntitlements(t.Context(), prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CheckEntitlements(t.Context(), params, WithIdempotencyKey("test-idempotency")); err != nil {
		t.Fatal(err)
	}
	invalid := params
	invalid.PrefixLimit = -1
	if _, err := client.CheckEntitlements(t.Context(), invalid); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("invalid check error = %v, want invalid request", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.CheckEntitlements(canceled, params); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cached check error = %v, want context canceled", err)
	}
	if count := requests.Load(); count != 6 {
		t.Fatalf("requests after bypass checks = %d, want 6", count)
	}
}

func TestEntitlementCheckCacheRequiresMetadataAndScope(t *testing.T) {
	for _, test := range []struct {
		name     string
		bound    bool
		metadata bool
	}{
		{name: "old server ignores negotiation", bound: true},
		{name: "merchant unresolved", metadata: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			mid := billing.MerchantID(uuid.New())
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				// Verify the SDK request remains understood by an old server
				// with a strict decoder.
				decoder := json.NewDecoder(r.Body)
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(new(billing.CheckEntitlementsParams)); err != nil {
					t.Errorf("legacy decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if test.bound {
					w.Header().Set(merchant.BoundHeader, mid.String())
				}
				out := billing.EntitlementCheck{}
				if test.metadata {
					out.EvaluatedAt = new(time.Now().UTC())
				}
				if err := json.NewEncoder(w).Encode(out); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer server.Close()

			client, err := NewRemoteMerchant(server.URL, RemoteCredential{APIKey: "test-key"}, RemoteMerchantOptions{
				EntitlementCheckCache: EntitlementCheckCacheOptions{MaxAge: time.Minute},
			})
			if err != nil {
				t.Fatal(err)
			}
			params := billing.CheckEntitlementsParams{CustomerID: billing.CustomerID(uuid.New()), Entitlements: []string{"premium"}}
			for range 2 {
				if _, err := client.CheckEntitlements(t.Context(), params); err != nil {
					t.Fatal(err)
				}
			}
			if count := requests.Load(); count != 2 {
				t.Fatalf("check requests = %d, want 2", count)
			}
		})
	}
}

func TestEntitlementCheckCacheDoesNotSkipTokenProvider(t *testing.T) {
	var requests, tokens atomic.Int32
	mid := billing.MerchantID(uuid.New())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get(entitlementCacheMetadataHeader) != "" {
			t.Error("dynamic credential requested cache metadata")
		}
		w.Header().Set(merchant.BoundHeader, mid.String())
		out := billing.EntitlementCheck{EvaluatedAt: new(time.Now().UTC())}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()
	client, err := NewRemoteMerchant(server.URL, RemoteCredential{Token: func(context.Context) (string, error) {
		tokens.Add(1)
		return "test-token", nil
	}}, RemoteMerchantOptions{EntitlementCheckCache: EntitlementCheckCacheOptions{MaxAge: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	params := billing.CheckEntitlementsParams{CustomerID: billing.CustomerID(uuid.New()), Entitlements: []string{"premium"}}
	for range 2 {
		if _, err := client.CheckEntitlements(t.Context(), params); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 2 || tokens.Load() != 2 {
		t.Fatalf("requests = %d, tokens = %d, want both 2", requests.Load(), tokens.Load())
	}
}

func TestEntitlementCheckCacheUsesRelativeLifetime(t *testing.T) {
	start := time.Now()
	serverNow := start.Add(-24 * time.Hour).UTC()
	for _, test := range []struct {
		name      string
		evaluated *time.Time
		valid     *time.Time
		lifetime  time.Duration
	}{
		{name: "legacy"},
		{name: "max age", evaluated: &serverNow, lifetime: 30 * time.Second},
		{name: "max age before boundary", evaluated: &serverNow, valid: new(serverNow.Add(time.Minute)), lifetime: 30 * time.Second},
		{name: "scheduled boundary", evaluated: &serverNow, valid: new(serverNow.Add(5 * time.Second)), lifetime: 5 * time.Second},
		{name: "expired", evaluated: &serverNow, valid: &serverNow},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := billing.EntitlementCheck{EvaluatedAt: test.evaluated, ValidUntil: test.valid}
			deadline := entitlementCheckDeadline(start, 30*time.Second, out)
			var want time.Time
			if test.lifetime != 0 {
				want = start.Add(test.lifetime)
			}
			if !deadline.Equal(want) {
				t.Fatalf("deadline = %v, want %v", deadline, want)
			}
		})
	}
}

func TestEntitlementCheckCacheExpiresAndEvicts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, err := newEntitlementCheckCache(EntitlementCheckCacheOptions{MaxAge: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		entry := entitlementCacheEntry{key: "expiring", body: []byte("test"), deadline: now.Add(time.Second)}
		cache.store(t.Context(), entry, entitlementRedisClock{})
		time.Sleep(time.Second)
		if got, _ := cache.lookup(t.Context(), entry.key); got.body != nil {
			t.Fatal("cache hit at the exclusive deadline")
		}

		cache.store(t.Context(), entitlementCacheEntry{key: "first", body: []byte(strings.Repeat("a", entitlementCacheMaxBytes)), deadline: now.Add(time.Minute)}, entitlementRedisClock{})
		cache.store(t.Context(), entitlementCacheEntry{key: "next", body: []byte("next"), deadline: now.Add(time.Minute)}, entitlementRedisClock{})
		if got, _ := cache.lookup(t.Context(), "first"); got.body != nil {
			t.Fatal("cache retained more data than its byte budget")
		}
	})
}

func TestEntitlementRedisBudgetLimitsOutstandingWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pending := make(chan struct{}, 1)
		release := make(chan struct{})
		ignoreCancellation := func(context.Context) int {
			<-release
			return 1
		}
		started := time.Now()
		if got := withEntitlementRedisBudget(t.Context(), pending, ignoreCancellation); got != 0 {
			t.Fatalf("lookup = %d, want miss", got)
		}
		if elapsed := time.Since(started); elapsed != entitlementCacheRedisTimeout {
			t.Fatalf("blocked lookup waited %v, want %v", elapsed, entitlementCacheRedisTimeout)
		}
		started = time.Now()
		if got := withEntitlementRedisBudget(t.Context(), pending, ignoreCancellation); got != 0 || time.Since(started) != 0 {
			t.Fatal("a full Redis work budget must fail immediately")
		}
		close(release)
	})
}
