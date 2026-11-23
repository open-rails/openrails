package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// entitlementServer answers checks bound to its merchant with answer, adding
// validity metadata when the request asks for it.
type entitlementServer struct {
	*httptest.Server
	merchant billing.MerchantID
	requests atomic.Int32
	validity atomic.Int32

	mu     sync.Mutex
	answer func(billing.CheckEntitlementsParams) billing.EntitlementCheck
}

func newEntitlementServer(t *testing.T, answer func(billing.CheckEntitlementsParams) billing.EntitlementCheck) *entitlementServer {
	t.Helper()
	s := &entitlementServer{merchant: billing.MerchantID(uuid.New()), answer: answer}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if r.URL.Path != entitlementCheckPath || r.Method != http.MethodPost {
			t.Errorf("unexpected entitlement check: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing application credential")
		}
		// The request decodes strictly, as the server's does.
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var params billing.CheckEntitlementsParams
		if err := decoder.Decode(&params); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if params.PrefixLimit < 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		out := s.answer(params)
		s.mu.Unlock()
		if params.IncludeValidity {
			s.validity.Add(1)
		} else {
			out.EvaluatedAt, out.ValidUntil = nil, nil
		}
		w.Header().Set(merchant.BoundHeader, s.merchant.String())
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *entitlementServer) setAnswer(answer func(billing.CheckEntitlementsParams) billing.EntitlementCheck) {
	s.mu.Lock()
	s.answer = answer
	s.mu.Unlock()
}

// grants answers every requested key: held keys with their quantity.
func grants(held map[string]*int) func(billing.CheckEntitlementsParams) billing.EntitlementCheck {
	return func(params billing.CheckEntitlementsParams) billing.EntitlementCheck {
		evaluated := time.Now().UTC().Add(-10 * time.Minute)
		out := billing.EntitlementCheck{Entitlements: map[string]bool{}, Quantities: map[string]*int{}, EvaluatedAt: &evaluated}
		for _, key := range params.Entitlements {
			quantity, ok := held[key]
			out.Entitlements[key] = ok
			out.Quantities[key] = quantity
		}
		return out
	}
}

func cachedClient(t *testing.T, server *entitlementServer, cache EntitlementCheckCacheOptions) *Client {
	t.Helper()
	client, err := NewRemoteMerchant(server.URL, RemoteCredential{APIKey: "test-key"}, RemoteMerchantOptions{EntitlementCheckCache: cache})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func check(t *testing.T, client *Client, customer billing.CustomerID, keys ...string) *billing.EntitlementCheck {
	t.Helper()
	out, err := client.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: keys})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func checkLive(t *testing.T, client *Client, customer billing.CustomerID, keys ...string) *billing.EntitlementCheck {
	t.Helper()
	out, err := client.CheckEntitlementsLive(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: keys})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func wantRequests(t *testing.T, server *entitlementServer, want int32) {
	t.Helper()
	if got := server.requests.Load(); got != want {
		t.Fatalf("requests = %d, want %d", got, want)
	}
}

func TestEntitlementCheckCacheIsOnByDefault(t *testing.T) {
	cache, err := newEntitlementCheckCache(EntitlementCheckCacheOptions{})
	if err != nil || cache == nil || cache.maxAge != DefaultEntitlementCheckMaxAge {
		t.Fatalf("zero options = %+v, %v; want the default cache", cache, err)
	}
	if cache, err := newEntitlementCheckCache(EntitlementCheckCacheOptions{Disabled: true, MaxAge: time.Minute}); cache != nil || err != nil {
		t.Fatalf("disabled cache = %+v, %v; want none", cache, err)
	}
	if _, err := newEntitlementCheckCache(EntitlementCheckCacheOptions{MaxAge: -time.Second}); err == nil {
		t.Fatal("negative MaxAge accepted")
	}

	server := newEntitlementServer(t, grants(map[string]*int{"premium": nil}))
	customer := billing.CustomerID(uuid.New())
	defaulted := cachedClient(t, server, EntitlementCheckCacheOptions{})
	for range 3 {
		if !check(t, defaulted, customer, "premium").Entitlements["premium"] {
			t.Fatal("premium denied")
		}
	}
	wantRequests(t, server, 1)

	disabled := cachedClient(t, server, EntitlementCheckCacheOptions{Disabled: true})
	for range 3 {
		if out := check(t, disabled, customer, "premium"); !out.Entitlements["premium"] || out.EvaluatedAt != nil {
			t.Fatalf("disabled check = %+v", out)
		}
	}
	wantRequests(t, server, 4)
	if got := server.validity.Load(); got != 1 {
		t.Fatalf("validity requests = %d, want only the default cache's", got)
	}
}

func TestEntitlementCheckCachesAllowedKeysOnly(t *testing.T) {
	server := newEntitlementServer(t, grants(map[string]*int{"premium": new(3)}))
	client := cachedClient(t, server, EntitlementCheckCacheOptions{MaxAge: 30 * time.Second})
	customer := billing.CustomerID(uuid.New())

	mixed := check(t, client, customer, "rental", "premium")
	if mixed.EvaluatedAt == nil || !mixed.Entitlements["premium"] || mixed.Entitlements["rental"] {
		t.Fatalf("mixed check = %+v", mixed)
	}
	if client.MerchantID() != server.merchant {
		t.Fatal("handle did not adopt the merchant from the first response")
	}
	// The denied key is never cached, so the same check asks again.
	check(t, client, customer, "premium", "rental")
	wantRequests(t, server, 2)

	// The allowed key alone is answered from the cache. Mutating a caller's
	// response cannot change the cache, and repeating a key changes nothing.
	allowed := check(t, client, customer, "premium")
	if !allowed.Entitlements["premium"] || *allowed.Quantities["premium"] != 3 || len(allowed.Held) != 0 || !allowed.EvaluatedAt.Equal(*mixed.EvaluatedAt) {
		t.Fatalf("cached check = %+v", allowed)
	}
	allowed.Entitlements["premium"] = false
	*allowed.Quantities["premium"] = 99
	*allowed.EvaluatedAt = time.Time{}
	again := check(t, client, customer, "premium", "premium")
	if !again.Entitlements["premium"] || *again.Quantities["premium"] != 3 || again.EvaluatedAt.IsZero() {
		t.Fatalf("second cached check = %+v", again)
	}
	wantRequests(t, server, 2)

	// Another customer shares nothing.
	check(t, client, billing.CustomerID(uuid.New()), "premium")
	wantRequests(t, server, 3)

	params := billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"premium"}}
	if out := checkLive(t, client, customer, "premium"); out.EvaluatedAt == nil || !out.Entitlements["premium"] {
		t.Fatalf("live check = %+v", out)
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
	wantRequests(t, server, 8)
}

func TestEntitlementCheckSeesPurchaseAtOnce(t *testing.T) {
	server := newEntitlementServer(t, grants(map[string]*int{"premium": nil}))
	client := cachedClient(t, server, EntitlementCheckCacheOptions{MaxAge: time.Minute})
	customer := billing.CustomerID(uuid.New())
	if out := check(t, client, customer, "premium", "rental"); out.Entitlements["rental"] {
		t.Fatal("rental granted before the purchase")
	}
	server.setAnswer(grants(map[string]*int{"premium": nil, "rental": nil}))
	if out := check(t, client, customer, "premium", "rental"); !out.Entitlements["rental"] || !out.Entitlements["premium"] {
		t.Fatalf("check after the purchase = %+v", out)
	}
	wantRequests(t, server, 2)
	check(t, client, customer, "rental", "premium")
	wantRequests(t, server, 2)
}

func TestEntitlementCheckLiveRevocationEvicts(t *testing.T) {
	server := newEntitlementServer(t, grants(map[string]*int{"premium": nil}))
	client := cachedClient(t, server, EntitlementCheckCacheOptions{MaxAge: time.Minute})
	customer := billing.CustomerID(uuid.New())
	check(t, client, customer, "premium")
	server.setAnswer(grants(nil))
	// Within MaxAge an unscheduled revocation is not yet seen...
	if !check(t, client, customer, "premium").Entitlements["premium"] {
		t.Fatal("cached allowed answer not served within MaxAge")
	}
	wantRequests(t, server, 1)
	// ...until a live check sees it and drops the entry.
	if checkLive(t, client, customer, "premium").Entitlements["premium"] {
		t.Fatal("live check missed the revocation")
	}
	if check(t, client, customer, "premium").Entitlements["premium"] {
		t.Fatal("revoked key served from the cache after a live check")
	}
	wantRequests(t, server, 3)

	// An ordinary fill also drops a key it finds denied.
	server.setAnswer(grants(map[string]*int{"premium": nil}))
	check(t, client, customer, "premium")
	server.setAnswer(grants(nil))
	check(t, client, customer, "premium", "rental")
	if check(t, client, customer, "premium").Entitlements["premium"] {
		t.Fatal("key a fill found denied served from the cache")
	}
	wantRequests(t, server, 6)
}

func TestEntitlementCheckLiveReplacesCachedAnswer(t *testing.T) {
	server := newEntitlementServer(t, grants(map[string]*int{"seats": new(1)}))
	client := cachedClient(t, server, EntitlementCheckCacheOptions{MaxAge: time.Minute})
	customer := billing.CustomerID(uuid.New())
	quantity := func(out *billing.EntitlementCheck) int {
		t.Helper()
		if !out.Entitlements["seats"] || out.Quantities["seats"] == nil {
			t.Fatalf("seats answer = %+v", out)
		}
		return *out.Quantities["seats"]
	}
	check(t, client, customer, "seats")
	server.setAnswer(grants(map[string]*int{"seats": new(2), "extra": nil}))
	// An ordinary fill never overwrites a valid entry...
	if got := quantity(check(t, client, customer, "seats", "extra")); got != 2 {
		t.Fatalf("filled quantity = %d, want the live 2", got)
	}
	if got := quantity(check(t, client, customer, "seats")); got != 1 {
		t.Fatalf("cached quantity = %d, want the first answer's 1", got)
	}
	// ...while a live check's answer replaces it.
	if got := quantity(checkLive(t, client, customer, "seats")); got != 2 {
		t.Fatalf("live quantity = %d, want 2", got)
	}
	if got := quantity(check(t, client, customer, "seats")); got != 2 {
		t.Fatalf("cached quantity after the live check = %d, want 2", got)
	}
	wantRequests(t, server, 3)
}

func TestEntitlementCheckComposesCachedKeys(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	answers := map[string]billing.EntitlementCheck{
		"a": {EvaluatedAt: new(base.Add(time.Second))},
		"b": {EvaluatedAt: new(base), ValidUntil: new(base.Add(time.Hour)), Quantities: map[string]*int{"b": new(5)}},
		"c": {EvaluatedAt: new(base.Add(2 * time.Second)), ValidUntil: new(base.Add(30 * time.Minute))},
	}
	server := newEntitlementServer(t, func(params billing.CheckEntitlementsParams) billing.EntitlementCheck {
		key := params.Entitlements[0]
		out := answers[key]
		out.Entitlements = map[string]bool{key: true}
		return out
	})
	client := cachedClient(t, server, EntitlementCheckCacheOptions{MaxAge: time.Minute})
	customer := billing.CustomerID(uuid.New())
	for _, key := range []string{"a", "b", "c"} {
		check(t, client, customer, key)
	}
	out := check(t, client, customer, "c", "a", "b")
	wantRequests(t, server, 3)
	if !out.Entitlements["a"] || !out.Entitlements["b"] || !out.Entitlements["c"] || len(out.Entitlements) != 3 {
		t.Fatalf("composed entitlements = %v", out.Entitlements)
	}
	if out.Quantities["a"] != nil || out.Quantities["c"] != nil || *out.Quantities["b"] != 5 || len(out.Held) != 0 {
		t.Fatalf("composed answer = %+v", out)
	}
	if !out.EvaluatedAt.Equal(base) || !out.ValidUntil.Equal(base.Add(30*time.Minute)) {
		t.Fatalf("composed evaluated_at = %v, valid_until = %v; want the oldest and the earliest", out.EvaluatedAt, out.ValidUntil)
	}
	if only := check(t, client, customer, "a"); only.ValidUntil != nil || !only.EvaluatedAt.Equal(base.Add(time.Second)) {
		t.Fatalf("single cached key = %+v", only)
	}
}

func TestEntitlementCheckCacheRequiresMetadataAndScope(t *testing.T) {
	for _, test := range []struct {
		name     string
		bound    bool
		metadata bool
	}{
		{name: "answer without metadata", bound: true},
		{name: "merchant unresolved", metadata: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			mid := billing.MerchantID(uuid.New())
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				decoder := json.NewDecoder(r.Body)
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(new(billing.CheckEntitlementsParams)); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if test.bound {
					w.Header().Set(merchant.BoundHeader, mid.String())
				}
				out := billing.EntitlementCheck{Entitlements: map[string]bool{"premium": true}}
				if test.metadata {
					out.EvaluatedAt = new(time.Now().UTC())
				}
				if err := json.NewEncoder(w).Encode(out); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer server.Close()

			client, err := NewRemoteMerchant(server.URL, RemoteCredential{APIKey: "test-key"})
			if err != nil {
				t.Fatal(err)
			}
			params := billing.CheckEntitlementsParams{CustomerID: billing.CustomerID(uuid.New()), Entitlements: []string{"premium"}}
			for range 2 {
				if out, err := client.CheckEntitlements(t.Context(), params); err != nil || !out.Entitlements["premium"] {
					t.Fatalf("check = %+v, %v", out, err)
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
		var params billing.CheckEntitlementsParams
		if err := json.NewDecoder(r.Body).Decode(&params); err != nil || params.IncludeValidity {
			t.Error("dynamic credential requested cache metadata")
		}
		w.Header().Set(merchant.BoundHeader, mid.String())
		out := billing.EntitlementCheck{Entitlements: map[string]bool{"premium": true}, EvaluatedAt: new(time.Now().UTC())}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()
	client, err := NewRemoteMerchant(server.URL, RemoteCredential{Token: func(context.Context) (string, error) {
		tokens.Add(1)
		return "test-token", nil
	}})
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
		entry := func(key string, lifetime time.Duration) entitlementCacheEntry {
			return entitlementCacheEntry{key: key, value: entitlementCacheValue{EvaluatedAt: now}, deadline: now.Add(lifetime)}
		}
		hit := func(keys ...string) bool {
			entries, _ := cache.lookup(t.Context(), keys)
			return entries != nil
		}
		cache.record(t.Context(), []entitlementCacheEntry{entry("expiring", time.Second), entry("kept", time.Minute)}, nil, entitlementRedisClock{}, false)
		time.Sleep(time.Second)
		if hit("expiring") || hit("kept", "expiring") || !hit("kept") {
			t.Fatal("cache hit at the exclusive deadline")
		}

		// The bound is bytes, not a count, and evicts the least recently used.
		cache.record(t.Context(), nil, []string{"kept"}, entitlementRedisClock{}, false)
		fits := entitlementCacheMaxBytes / entry("k00000", 0).size()
		for i := range fits {
			cache.record(t.Context(), []entitlementCacheEntry{entry(fmt.Sprintf("k%05d", i), time.Minute)}, nil, entitlementRedisClock{}, false)
		}
		if len(cache.entries) != fits || !hit("k00000") {
			t.Fatalf("entries = %d, want all %d within the byte budget", len(cache.entries), fits)
		}
		cache.record(t.Context(), []entitlementCacheEntry{entry("next", time.Minute)}, nil, entitlementRedisClock{}, false)
		if hit("k00001") || !hit("k00000") || !hit("next") || cache.bytes > entitlementCacheMaxBytes {
			t.Fatal("cache did not evict the least recently used entry at its byte budget")
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
