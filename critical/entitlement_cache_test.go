//go:build integration

package critical_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type entitlementWire struct {
	http.RoundTripper
	requests    atomic.Int64
	unreachable atomic.Bool
	// stall, when set, runs once after a check's answer arrives, before the
	// client sees it.
	stall atomic.Pointer[func()]
}

func (w *entitlementWire) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path != "/v1/app/entitlements/check" {
		return w.RoundTripper.RoundTrip(r)
	}
	w.requests.Add(1)
	if w.unreachable.Load() {
		return nil, errors.New("critical fixture cannot reach OpenRails")
	}
	response, err := w.RoundTripper.RoundTrip(r)
	if stall := w.stall.Swap(nil); stall != nil && err == nil {
		(*stall)()
	}
	return response, err
}

func newEntitlementClient(t *testing.T, a *authorityWorld, merchant billing.MerchantID, key string, cache openrails.EntitlementCheckCacheOptions) (*openrails.Client, *entitlementWire) {
	t.Helper()
	wire := &entitlementWire{RoundTripper: http.DefaultTransport}
	remote, err := openrails.NewRemote(a.server.URL, openrails.RemoteOptions{
		HTTPClient:            &http.Client{Transport: wire, Timeout: 5 * time.Second},
		Credentials:           map[billing.MerchantID]openrails.RemoteCredential{merchant: {APIKey: key}},
		EntitlementCheckCache: cache,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remote.Close(context.Background())) })
	client, err := remote.Merchant(merchant)
	require.NoError(t, err)
	return client, wire
}

// newCachedEntitlementClient caches for age; zero disables the cache.
func newCachedEntitlementClient(t *testing.T, a *authorityWorld, merchant billing.MerchantID, key string, age time.Duration, store redis.UniversalClient) (*openrails.Client, *entitlementWire) {
	t.Helper()
	return newEntitlementClient(t, a, merchant, key, openrails.EntitlementCheckCacheOptions{MaxAge: age, Disabled: age == 0, Redis: store})
}

func TestRemoteEntitlementCache(t *testing.T) {
	t.Run("OR-J27-S13/merchant-cache-reuses-only-current-scoped-access", func(t *testing.T) {
		a := newAuthorityWorld(t)
		grant := publicGrant(t, a, "cache-rental", 1)
		mixed := billing.CheckEntitlementsParams{
			CustomerID: grant.CustomerID, Entitlements: []string{"library:read", "never:granted"},
		}
		held := billing.CheckEntitlementsParams{CustomerID: grant.CustomerID, Entitlements: []string{"library:read"}}
		assertMixed := func(client *openrails.Client, allowed bool) *billing.EntitlementCheck {
			t.Helper()
			check, err := client.CheckEntitlements(t.Context(), mixed)
			require.NoError(t, err)
			require.Equal(t, allowed, check.Entitlements["library:read"])
			require.Contains(t, check.Entitlements, "never:granted")
			require.False(t, check.Entitlements["never:granted"])
			return check
		}
		assertHeld := func(client *openrails.Client, allowed bool) *billing.EntitlementCheck {
			t.Helper()
			check, err := client.CheckEntitlements(t.Context(), held)
			require.NoError(t, err)
			require.Equal(t, allowed, check.Entitlements["library:read"])
			return check
		}

		// A mixed check caches its allowed key only: repeating it asks again,
		// while the allowed key alone is answered from the cache.
		merchant := a.alpha.MerchantID()
		local, localWire := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 30*time.Second, nil)
		started := time.Now()
		cold := assertMixed(local, true)
		coldLatency := time.Since(started)
		require.NotNil(t, cold.EvaluatedAt)
		require.NotNil(t, cold.ValidUntil)
		require.True(t, cold.ValidUntil.Equal(*grant.EndsAt))
		assertMixed(local, true)
		require.EqualValues(t, 2, localWire.requests.Load())
		const samples = 100
		started = time.Now()
		for range samples {
			check := assertHeld(local, true)
			require.True(t, check.EvaluatedAt.Equal(*cold.EvaluatedAt))
			require.True(t, check.ValidUntil.Equal(*grant.EndsAt))
		}
		t.Logf("in-process cold %s, mean warm %s over %d checks, HTTP requests %d",
			coldLatency, time.Since(started)/samples, samples, localWire.requests.Load())
		require.EqualValues(t, 2, localWire.requests.Load())

		uncached, uncachedWire := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 0, nil)
		started = time.Now()
		for range samples {
			check := assertHeld(uncached, true)
			require.Nil(t, check.EvaluatedAt)
		}
		t.Logf("cache disabled, mean %s over %d checks, HTTP requests %d",
			time.Since(started)/samples, samples, uncachedWire.requests.Load())
		require.EqualValues(t, samples, uncachedWire.requests.Load())

		// Over Redis, one replica's mixed check fills the allowed key's entry
		// for its peer; the denied key is never shared. A Redis call can rarely
		// miss its 25 ms budget and fall back to OpenRails, so the warm peer may
		// ask a few times, never most.
		sharedA, sharedWireA := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 30*time.Second, criticalRedis(t))
		replicaRedis := criticalRedis(t)
		sharedB, sharedWireB := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 30*time.Second, replicaRedis)
		assertMixed(sharedA, true)
		started = time.Now()
		for range samples {
			check := assertHeld(sharedB, true)
			require.True(t, check.ValidUntil.Equal(*grant.EndsAt))
		}
		t.Logf("Redis mean warm %s over %d checks, producer HTTP requests %d, peer HTTP requests %d",
			time.Since(started)/samples, samples, sharedWireA.requests.Load(), sharedWireB.requests.Load())
		require.EqualValues(t, 1, sharedWireA.requests.Load())
		require.LessOrEqual(t, sharedWireB.requests.Load(), int64(samples/10))
		peerRequests := sharedWireB.requests.Load()
		assertMixed(sharedB, true)
		require.Equal(t, peerRequests+1, sharedWireB.requests.Load())
		require.NoError(t, replicaRedis.Close())
		assertHeld(sharedB, true)
		require.Equal(t, peerRequests+2, sharedWireB.requests.Load())

		readOnlyKey, err := a.auth.CreateAPIKey(t.Context(), iam.SystemIdentity(), a.alphaGroup, iam.NewAPIKey{Name: "customer-reader", Role: a.reader})
		require.NoError(t, err)
		outsider, outsiderWire := newCachedEntitlementClient(t, a, merchant, readOnlyKey.Secret, 30*time.Second, criticalRedis(t))
		_, err = outsider.CheckEntitlements(t.Context(), held)
		require.Error(t, err)
		require.EqualValues(t, 1, outsiderWire.requests.Load())
		otherMerchant, otherWire := newCachedEntitlementClient(t, a, a.beta.MerchantID(), a.betaKey.Secret, 30*time.Second, criticalRedis(t))
		assertHeld(otherMerchant, false)
		require.EqualValues(t, 1, otherWire.requests.Load())

		history := held
		history.At = grant.StartsAt.Add(-time.Microsecond)
		historical, err := local.CheckEntitlements(t.Context(), history)
		require.NoError(t, err)
		require.False(t, historical.Entitlements["library:read"])
		require.Nil(t, historical.EvaluatedAt)
		require.EqualValues(t, 3, localWire.requests.Load())

		a.clock.Advance(grant.EndsAt.Add(-time.Second).Sub(a.clock.Now()))
		nearEnd, nearEndWire := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 30*time.Second, nil)
		assertMixed(nearEnd, true)
		assertHeld(nearEnd, true)
		require.EqualValues(t, 1, nearEndWire.requests.Load())
		a.clock.Advance(time.Second)
		nearEndWire.unreachable.Store(true)
		require.Eventually(t, func() bool {
			_, err := nearEnd.CheckEntitlements(t.Context(), held)
			return errors.Is(err, billing.ErrUnreachable)
		}, 2*time.Second, 25*time.Millisecond)
		require.EqualValues(t, 2, nearEndWire.requests.Load())
		nearEndWire.unreachable.Store(false)
		assertHeld(nearEnd, false)
		require.EqualValues(t, 3, nearEndWire.requests.Load())

		// An unplanned revocation is immediately visible on the live path,
		// which drops the entry, so the next ordinary check asks again.
		revocable := publicGrant(t, a, "cache-revocation", 2)
		a.clock.Advance(time.Millisecond)
		short, shortWire := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 2*time.Second, nil)
		assertHeld(short, true)
		require.NoError(t, a.alphaRemote.RevokeProductAccess(t.Context(), revocable.ID, billing.RevokeProductAccessParams{Reason: "revoke access early"}))
		live, err := short.Uncached().CheckEntitlements(t.Context(), held)
		require.NoError(t, err)
		require.False(t, live.Entitlements["library:read"])
		assertHeld(short, false)
		require.EqualValues(t, 3, shortWire.requests.Load())

		// Over a shared Redis, one replica's live denial drops the entry for
		// every replica, and a peer's fill begun before it cannot restore it.
		// A Redis call that misses its budget can spoil an attempt, so the
		// race runs once more before failing; a real defect fails both. Each
		// attempt's MaxAge keeps its entries apart from the ones above, which
		// Redis still holds in real time.
		revokedEverywhere := func(attempt int) bool {
			t.Helper()
			granted := publicGrant(t, a, fmt.Sprintf("cache-revocation-shared-%d", attempt), 3)
			revoke := func() {
				t.Helper()
				require.NoError(t, a.alphaRemote.RevokeProductAccess(t.Context(), granted.ID, billing.RevokeProductAccessParams{Reason: "revoke shared access early"}))
			}
			a.clock.Advance(time.Millisecond)
			age := time.Duration(45+attempt) * time.Second
			redisA, _ := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, age, criticalRedis(t))
			redisB, redisWireB := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, age, criticalRedis(t))
			assertHeld(redisA, true)
			assertHeld(redisB, true)
			if redisWireB.requests.Load() != 0 {
				revoke()
				return false
			}
			// B's mixed check is answered before the revocation, but its fill
			// lands only after A's live check has seen the revocation.
			evaluated, release := make(chan struct{}), make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseOnce)
			stall := func() {
				close(evaluated)
				<-release
			}
			redisWireB.stall.Store(&stall)
			inFlight := make(chan error, 1)
			go func() {
				check, err := redisB.CheckEntitlements(t.Context(), mixed)
				if err == nil && !check.Entitlements["library:read"] {
					err = errors.New("in-flight check was answered after the revocation")
				}
				inFlight <- err
			}()
			<-evaluated
			revoke()
			live, err := redisA.Uncached().CheckEntitlements(t.Context(), held)
			require.NoError(t, err)
			require.False(t, live.Entitlements["library:read"])
			releaseOnce()
			require.NoError(t, <-inFlight)
			for range 2 {
				before := redisWireB.requests.Load()
				check, err := redisB.CheckEntitlements(t.Context(), held)
				require.NoError(t, err)
				if check.Entitlements["library:read"] {
					return false
				}
				require.Equal(t, before+1, redisWireB.requests.Load(), "a denial is never cached")
			}
			return true
		}
		if !revokedEverywhere(0) {
			t.Log("a Redis call missed its budget; running the revocation race once more")
			require.True(t, revokedEverywhere(1))
		}
	})
	t.Run("OR-J27-S14/a-purchase-after-a-cached-check-is-visible-on-the-next-check", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		publicGrant(t, a, "cache-held", 24)
		product, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "cache-purchase", DisplayName: "Cache purchase", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"purchase:owned"}})
		require.NoError(t, err)
		price, err := a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "cache-purchase", UnitAmount: 9_990_000, Currency: "USD", AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		both := billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"library:read", "purchase:owned"}}
		held := billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"library:read"}}
		verdict := func(client *openrails.Client, params billing.CheckEntitlementsParams) map[string]bool {
			t.Helper()
			check, err := client.CheckEntitlements(t.Context(), params)
			require.NoError(t, err)
			return check.Entitlements
		}

		// Zero options are the default cache, in process and over Redis. A
		// Redis call can rarely miss its budget and ask OpenRails instead.
		local, localWire := newEntitlementClient(t, a, a.alpha.MerchantID(), a.alphaKey.Secret, openrails.EntitlementCheckCacheOptions{})
		shared, sharedWire := newEntitlementClient(t, a, a.alpha.MerchantID(), a.alphaKey.Secret, openrails.EntitlementCheckCacheOptions{Redis: criticalRedis(t)})
		for _, client := range []*openrails.Client{local, shared} {
			require.Equal(t, map[string]bool{"library:read": true, "purchase:owned": false}, verdict(client, both))
		}
		const samples = 20
		for range samples {
			for _, client := range []*openrails.Client{local, shared} {
				require.Equal(t, map[string]bool{"library:read": true}, verdict(client, held))
			}
		}
		require.EqualValues(t, 1, localWire.requests.Load())
		require.LessOrEqual(t, sharedWire.requests.Load(), int64(1+samples/10))

		// The purchased key was never cached, so the next check asks and sees it.
		publicPurchase(t, a, p, price.ID, 1, 9_990_000, "cache-purchase")
		for _, item := range []struct {
			client *openrails.Client
			wire   *entitlementWire
		}{{local, localWire}, {shared, sharedWire}} {
			before := item.wire.requests.Load()
			require.Equal(t, map[string]bool{"library:read": true, "purchase:owned": true}, verdict(item.client, both))
			require.Equal(t, before+1, item.wire.requests.Load())
			require.Equal(t, map[string]bool{"library:read": true, "purchase:owned": true}, verdict(item.client, both))
		}
		require.EqualValues(t, 2, localWire.requests.Load())
	})
}
