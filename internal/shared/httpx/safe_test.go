package httpx

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", // loopback
		"10.0.0.1", "172.16.0.1", "192.168.1.1", // RFC1918
		"169.254.169.254", "fe80::1", // link-local (cloud metadata)
		"100.64.0.1", "100.127.255.254", // CGNAT
		"fc00::1", "fd00::1", // ULA
		"0.0.0.0", "255.255.255.255", "224.0.0.1",
		"192.0.0.170", "198.18.0.1", // NAT64 / benchmarking
		"::ffff:127.0.0.1", // IPv4-mapped loopback
	}
	for _, s := range blocked {
		require.True(t, IsBlockedIP(net.ParseIP(s)), "%s must be blocked", s)
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111"} {
		require.False(t, IsBlockedIP(net.ParseIP(s)), "%s must be allowed", s)
	}
	require.True(t, IsBlockedIP(nil))
}

func TestValidateURL(t *testing.T) {
	strict := Policy{}
	for _, bad := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/admin",
		"https://[::1]/",
		"http://100.64.3.4/",
		"http://localhost:5432/",
		"file:///etc/passwd",
		"gopher://x/",
		"://nope",
	} {
		require.Error(t, strict.ValidateURL(bad), "%s must be rejected", bad)
	}
	require.NoError(t, strict.ValidateURL("https://hooks.example.com/alerts"))

	// The test escape hatch re-admits loopback and nothing else.
	loop := Policy{Allow: AllowLoopback}
	require.NoError(t, loop.ValidateURL("http://127.0.0.1:8080/x"))
	require.Error(t, loop.ValidateURL("http://169.254.169.254/"))
}

// #SEC-21: the guard is at the DIALER, so a HOSTNAME that resolves to an
// internal address is blocked at connect time — the request never leaves. This
// is the DNS-rebinding-proof placement: no parse-time host check can do it.
func TestSafeClientBlocksResolvedInternalAddress(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	client := Policy{}.Client(3 * time.Second)
	// Literal loopback IP...
	_, err := client.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrBlockedAddress)
	// ...and a NAME that resolves to it: only a post-DNS check catches this.
	_, err = client.Post(fmt.Sprintf("http://localhost:%d/", port), "application/json", strings.NewReader(`{}`))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrBlockedAddress)

	require.Zero(t, atomic.LoadInt32(&hits), "no request may reach an internal address")
}

// A reachable public host must not be able to 302 the client into link-local.
func TestSafeClientRevalidatesRedirectHops(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	// The origin is allowed (stands in for a public host); the redirect target
	// is not — and the policy re-checks every hop.
	client := Policy{Allow: AllowLoopback}.Client(3 * time.Second)
	_, err := client.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	require.Error(t, err)
	var uerr *url.Error
	require.ErrorAs(t, err, &uerr)
	require.ErrorIs(t, err, ErrBlockedAddress)
	require.EqualValues(t, 1, atomic.LoadInt32(&hits), "only the origin is hit; the redirect is not followed")
}

func TestFailureDetailIsNotAnOracle(t *testing.T) {
	// A raw dial error would say "connection refused" vs "i/o timeout" —
	// enough to map an internal network. It must not survive.
	detail := FailureDetail(fmt.Errorf(`Post "http://10.1.2.3:6379/": dial tcp 10.1.2.3:6379: connect: connection refused`))
	require.Equal(t, "delivery failed: the destination could not be reached", detail)
	require.NotContains(t, detail, "10.1.2.3")
	require.Equal(t, "destination address is not publicly routable", FailureDetail(ErrBlockedAddress))
	require.Empty(t, FailureDetail(nil))
}
