//go:build integration

package critical_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type entitlementWire struct {
	http.RoundTripper
	requests atomic.Int64
}

func (w *entitlementWire) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/v1/app/entitlements/check" {
		w.requests.Add(1)
	}
	return w.RoundTripper.RoundTrip(r)
}

func newCachedEntitlementClient(t *testing.T, a *authorityWorld, merchant billing.MerchantID, key string, age time.Duration, store redis.UniversalClient) (*openrails.Client, *entitlementWire) {
	t.Helper()
	wire := &entitlementWire{RoundTripper: http.DefaultTransport}
	remote, err := openrails.NewRemote(a.server.URL, openrails.RemoteOptions{
		HTTPClient:  &http.Client{Transport: wire, Timeout: 5 * time.Second},
		Credentials: map[billing.MerchantID]openrails.RemoteCredential{merchant: {APIKey: key}},
		EntitlementCheckCache: openrails.EntitlementCheckCacheOptions{
			MaxAge: age,
			Redis:  store,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remote.Close(context.Background())) })
	client, err := remote.Merchant(merchant)
	require.NoError(t, err)
	return client, wire
}

func TestRemoteEntitlementCache(t *testing.T) {
	t.Run("OR-J27-S13/merchant-cache-reuses-only-current-scoped-access", func(t *testing.T) {
		a := newAuthorityWorld(t)
		grant := publicGrant(t, a, "cache-rental", 1)
		params := billing.CheckEntitlementsParams{
			CustomerID: grant.CustomerID, Entitlements: []string{"library:read", "never:granted"},
		}
		assertVerdict := func(client *openrails.Client, allowed bool) *billing.EntitlementCheck {
			t.Helper()
			check, err := client.CheckEntitlements(t.Context(), params)
			require.NoError(t, err)
			require.Equal(t, allowed, check.Entitlements["library:read"])
			require.Contains(t, check.Entitlements, "never:granted")
			require.False(t, check.Entitlements["never:granted"])
			return check
		}

		merchant := a.alpha.MerchantID()
		local, localWire := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 30*time.Second, nil)
		started := time.Now()
		cold := assertVerdict(local, true)
		coldLatency := time.Since(started)
		require.NotNil(t, cold.EvaluatedAt)
		require.NotNil(t, cold.ValidUntil)
		require.True(t, cold.ValidUntil.Equal(*grant.EndsAt))
		const samples = 100
		started = time.Now()
		for range samples {
			check, err := local.CheckEntitlements(t.Context(), params)
			require.NoError(t, err)
			require.True(t, check.Entitlements["library:read"])
		}
		t.Logf("in-process cold %s, mean warm %s over %d checks, HTTP requests %d",
			coldLatency, time.Since(started)/samples, samples, localWire.requests.Load())
		require.EqualValues(t, 1, localWire.requests.Load())

		uncached, uncachedWire := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 0, nil)
		started = time.Now()
		for range samples {
			check, err := uncached.CheckEntitlements(t.Context(), params)
			require.NoError(t, err)
			require.True(t, check.Entitlements["library:read"])
		}
		t.Logf("cache disabled, mean %s over %d checks, HTTP requests %d",
			time.Since(started)/samples, samples, uncachedWire.requests.Load())
		require.EqualValues(t, samples, uncachedWire.requests.Load())

		sharedA, sharedWireA := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 30*time.Second, criticalRedis(t))
		sharedB, sharedWireB := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 30*time.Second, criticalRedis(t))
		assertVerdict(sharedA, true)
		started = time.Now()
		for range samples {
			check, err := sharedB.CheckEntitlements(t.Context(), params)
			require.NoError(t, err)
			require.True(t, check.Entitlements["library:read"])
		}
		t.Logf("Redis mean warm %s over %d checks, producer HTTP requests %d, peer HTTP requests %d",
			time.Since(started)/samples, samples, sharedWireA.requests.Load(), sharedWireB.requests.Load())
		require.EqualValues(t, 1, sharedWireA.requests.Load())
		require.Zero(t, sharedWireB.requests.Load())

		readOnlyKey, err := a.auth.CreateAPIKey(t.Context(), iam.SystemIdentity(), a.alphaGroup, iam.NewAPIKey{Name: "customer-reader", Role: a.reader})
		require.NoError(t, err)
		outsider, outsiderWire := newCachedEntitlementClient(t, a, merchant, readOnlyKey.Secret, 30*time.Second, criticalRedis(t))
		_, err = outsider.CheckEntitlements(t.Context(), params)
		require.Error(t, err)
		require.EqualValues(t, 1, outsiderWire.requests.Load())
		otherMerchant, otherWire := newCachedEntitlementClient(t, a, a.beta.MerchantID(), a.betaKey.Secret, 30*time.Second, criticalRedis(t))
		assertVerdict(otherMerchant, false)
		require.EqualValues(t, 1, otherWire.requests.Load())

		history := params
		history.At = grant.StartsAt.Add(-time.Microsecond)
		historical, err := local.CheckEntitlements(t.Context(), history)
		require.NoError(t, err)
		require.False(t, historical.Entitlements["library:read"])
		require.Nil(t, historical.EvaluatedAt)
		require.EqualValues(t, 2, localWire.requests.Load())

		a.clock.Advance(grant.EndsAt.Add(-time.Second).Sub(a.clock.Now()))
		nearEnd, nearEndWire := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 30*time.Second, nil)
		assertVerdict(nearEnd, true)
		assertVerdict(nearEnd, true)
		require.EqualValues(t, 1, nearEndWire.requests.Load())
		a.clock.Advance(time.Second)
		require.Eventually(t, func() bool {
			current, err := nearEnd.CheckEntitlements(t.Context(), params)
			return err == nil && !current.Entitlements["library:read"]
		}, 2*time.Second, 25*time.Millisecond)
		require.EqualValues(t, 2, nearEndWire.requests.Load())

		// An unplanned revocation is immediately visible on the live path.
		revocable := publicGrant(t, a, "cache-revocation", 2)
		a.clock.Advance(time.Millisecond)
		short, _ := newCachedEntitlementClient(t, a, merchant, a.alphaKey.Secret, 2*time.Second, nil)
		assertVerdict(short, true)
		require.NoError(t, a.alphaRemote.RevokeProductAccess(t.Context(), revocable.ID, billing.RevokeProductAccessParams{Reason: "revoke access early"}))
		live, err := short.CheckEntitlementsLive(t.Context(), params)
		require.NoError(t, err)
		require.False(t, live.Entitlements["library:read"])
		require.Eventually(t, func() bool {
			current, err := short.CheckEntitlements(t.Context(), params)
			return err == nil && !current.Entitlements["library:read"]
		}, 3*time.Second, 50*time.Millisecond)
	})
}
