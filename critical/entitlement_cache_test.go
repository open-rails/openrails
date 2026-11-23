//go:build integration

package critical_test

import (
	"context"
	"errors"
	"net/http"
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
}

func (w *entitlementWire) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/v1/app/entitlements/check" {
		w.requests.Add(1)
		if w.unreachable.Load() {
			return nil, errors.New("critical fixture cannot reach OpenRails")
		}
	}
	return w.RoundTripper.RoundTrip(r)
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
		// for its peer; the denied key is never shared.
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
		require.Zero(t, sharedWireB.requests.Load())
		assertMixed(sharedB, true)
		require.EqualValues(t, 1, sharedWireB.requests.Load())
		require.NoError(t, replicaRedis.Close())
		assertHeld(sharedB, true)
		require.EqualValues(t, 2, sharedWireB.requests.Load())

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
		live, err := short.CheckEntitlementsLive(t.Context(), held)
		require.NoError(t, err)
		require.False(t, live.Entitlements["library:read"])
		assertHeld(short, false)
		require.EqualValues(t, 3, shortWire.requests.Load())

		// Over a shared Redis, one replica's live answer drops the entry for
		// every replica. A distinct MaxAge keeps these entries apart from the
		// ones above, which Redis still holds in real time.
		shared := publicGrant(t, a, "cache-revocation-shared", 3)
		a.clock.Advance(time.Millisecond)
		redisA, _ := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 45*time.Second, criticalRedis(t))
		redisB, redisWireB := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 45*time.Second, criticalRedis(t))
		assertHeld(redisA, true)
		assertHeld(redisB, true)
		require.Zero(t, redisWireB.requests.Load())
		require.NoError(t, a.alphaRemote.RevokeProductAccess(t.Context(), shared.ID, billing.RevokeProductAccessParams{Reason: "revoke shared access early"}))
		live, err = redisA.CheckEntitlementsLive(t.Context(), held)
		require.NoError(t, err)
		require.False(t, live.Entitlements["library:read"])
		assertHeld(redisB, false)
		require.EqualValues(t, 1, redisWireB.requests.Load())
		assertHeld(redisB, false)
		require.EqualValues(t, 2, redisWireB.requests.Load())
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

		// Zero options are the default cache, in process and over Redis.
		local, localWire := newEntitlementClient(t, a, a.alpha.MerchantID(), a.alphaKey.Secret, openrails.EntitlementCheckCacheOptions{})
		shared, sharedWire := newEntitlementClient(t, a, a.alpha.MerchantID(), a.alphaKey.Secret, openrails.EntitlementCheckCacheOptions{Redis: criticalRedis(t)})
		for _, client := range []*openrails.Client{local, shared} {
			require.Equal(t, map[string]bool{"library:read": true, "purchase:owned": false}, verdict(client, both))
			require.Equal(t, map[string]bool{"library:read": true}, verdict(client, held))
		}
		require.EqualValues(t, 1, localWire.requests.Load())
		require.EqualValues(t, 1, sharedWire.requests.Load())

		publicPurchase(t, a, p, price.ID, 1, 9_990_000, "cache-purchase")
		for _, client := range []*openrails.Client{local, shared} {
			require.Equal(t, map[string]bool{"library:read": true, "purchase:owned": true}, verdict(client, both))
			require.Equal(t, map[string]bool{"library:read": true, "purchase:owned": true}, verdict(client, both))
		}
		require.EqualValues(t, 2, localWire.requests.Load())
		require.EqualValues(t, 2, sharedWire.requests.Load())
	})
}
