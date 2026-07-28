package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	authcore "github.com/open-rails/authkit/embedded"
)

// waitRefreshIdle waits for the single-flighted background refresh to release
// `busy`. A panic in that goroutine is unrecovered by the test framework — it
// kills the whole test binary — so "this returns at all" IS the assertion.
func waitRefreshIdle(t *testing.T, c *ControlPlane) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.issuerRefresh.busy.Load() {
		if time.Now().After(deadline) {
			t.Fatal("issuer-registry refresh never released busy")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// or#854: a ControlPlane with a delegated verifier but NO core client (the state
// every controlplane unit test builds: &ControlPlane{delegatedVerifier: v}) used
// to pass a typed-nil *authcore.Client into the verifier's
// RemoteApplicationSource interface. The boxed nil is a non-NIL interface, so
// authkit's own `src == nil` fallback never fired and ListRemoteApplications
// nil-dereffed — inside an unrecovered background goroutine, i.e. process death.
func TestLoadRemoteApplications_NoCoreClientDegrades(t *testing.T) {
	v, _ := newTestDelegatedVerifier(t)
	cp := &ControlPlane{delegatedVerifier: v}

	err := cp.loadRemoteApplications(context.Background())
	require.ErrorIs(t, err, ErrRemoteApplicationSourceUnavailable)
	require.Zero(t, cp.issuerRefresh.lastLoad.Load(), "a failed load must leave lastLoad unstamped so the next verification retries")
}

// The TTL refresh path in the same partially-configured state must degrade
// silently instead of crashing the process.
func TestRefreshIssuerRegistryIfStale_NoCoreClientDoesNotPanic(t *testing.T) {
	v, _ := newTestDelegatedVerifier(t)
	cp := &ControlPlane{delegatedVerifier: v}
	cp.SetIssuerRegistryTTL(time.Nanosecond) // always stale

	cp.refreshIssuerRegistryIfStale()
	waitRefreshIdle(t, cp)

	require.Zero(t, cp.issuerRefresh.lastLoad.Load(), "registry stays stale; nothing was loaded")
	require.False(t, cp.issuerRefresh.busy.Load(), "single-flight must be released")
}

// The load-bearing half of or#854: whatever future nil appears inside the
// refresh, the background goroutine must recover rather than tear down the
// binary. A zero-value *authcore.Client is a non-nil source whose backing
// service is nil, so the load panics deep inside authkit — past every guard we
// could write here. The process must survive it.
func TestRefreshIssuerRegistryIfStale_RecoversPanic(t *testing.T) {
	v, _ := newTestDelegatedVerifier(t)
	cp := &ControlPlane{delegatedVerifier: v, authClient: &authcore.Client{}}
	cp.SetIssuerRegistryTTL(time.Nanosecond)

	cp.refreshIssuerRegistryIfStale()
	waitRefreshIdle(t, cp)

	require.Zero(t, cp.issuerRefresh.lastLoad.Load(), "a panicking refresh must leave lastLoad unstamped so the next verification retries")
	require.False(t, cp.issuerRefresh.busy.Load(), "single-flight must be released after a recovered panic")

	// And it must still be re-armable: a second kick also survives.
	cp.refreshIssuerRegistryIfStale()
	waitRefreshIdle(t, cp)
}
