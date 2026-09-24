package httpx

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDecodeJSONLimited(t *testing.T) {
	var out map[string]any
	require.NoError(t, DecodeJSONLimited(strings.NewReader(`{"a":1}`), 7, &out), "exactly at the cap")
	require.EqualValues(t, 1, out["a"])
	require.ErrorIs(t, DecodeJSONLimited(strings.NewReader(`{"a":10}`), 7, &out), ErrResponseTooLarge)
	require.NoError(t, DecodeJSONLimited(strings.NewReader(`{"k":"v"}`), 0, &out), "non-positive cap uses the default")
	big := `"` + strings.Repeat("a", int(DefaultMaxResponseBytes)) + `"`
	require.ErrorIs(t, DecodeJSONLimited(strings.NewReader(big), -1, &out), ErrResponseTooLarge)
	require.Error(t, DecodeJSONLimited(nil, 0, &out))
	err := DecodeJSONLimited(strings.NewReader(`{not json`), 0, &out)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrResponseTooLarge))
}

// #SEC-21: only publicly routable unicast destinations are reachable.
func TestIsBlockedIP(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "::1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "fe80::1", "100.64.0.1", "100.127.255.254", "fc00::1", "fd00::1",
		"0.0.0.0", "::", "0.1.2.3", "255.255.255.255", "224.0.0.1", "ff02::1",
		"192.0.0.170", "198.18.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1",
		"64:ff9b::a00:1", "2001:db8::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
	} {
		require.True(t, IsBlockedIP(net.ParseIP(s)), "%s must be blocked", s)
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "100.128.0.1", "2606:4700::1111"} {
		require.False(t, IsBlockedIP(net.ParseIP(s)), "%s must be allowed", s)
	}
	require.True(t, IsBlockedIP(nil))
}

func TestValidateURL(t *testing.T) {
	strict, loop := Policy{}, Policy{Allow: AllowLoopback}
	for _, tc := range []struct {
		url           string
		strict, loopb error
	}{
		{"https://hooks.example.com/alerts", nil, nil},
		{"http://127.0.0.1:8080/admin", ErrBlockedAddress, nil},
		{"https://[::1]/", ErrBlockedAddress, nil},
		{"http://LOCALHOST:5432/", ErrBlockedAddress, nil},
		{"http://db.localhost/", ErrBlockedAddress, nil},
		{"http://169.254.169.254/latest/meta-data/", ErrBlockedAddress, ErrBlockedAddress},
		{"http://100.64.3.4/", ErrBlockedAddress, ErrBlockedAddress},
		{"file:///etc/passwd", ErrUnsupportedScheme, ErrUnsupportedScheme},
		{"gopher://x/", ErrUnsupportedScheme, ErrUnsupportedScheme},
	} {
		for _, c := range []struct {
			p    Policy
			want error
		}{{strict, tc.strict}, {loop, tc.loopb}} {
			err := c.p.ValidateURL(tc.url)
			if c.want == nil {
				require.NoError(t, err, tc.url)
			} else {
				require.ErrorIs(t, err, c.want, tc.url)
			}
		}
	}
	require.Error(t, strict.ValidateURL("://nope"))
	require.Error(t, strict.ValidateURL("http:///no-host"))
}

// The guard sits at the dialer (post-DNS) and on every redirect hop, so neither
// a name resolving internally nor a public host's 302 reaches an internal address.
func TestClientEnforcesPolicyAtDialAndRedirect(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	strict := Policy{}.Client(3 * time.Second)
	for _, target := range []string{srv.URL, fmt.Sprintf("http://localhost:%d/", port)} {
		_, err := strict.Post(target, "application/json", strings.NewReader(`{}`))
		require.ErrorIs(t, err, ErrBlockedAddress, target)
	}
	require.Zero(t, hits.Load(), "no request may reach an internal address")

	_, err := Policy{Allow: AllowLoopback}.Client(3 * time.Second).Get(srv.URL)
	require.ErrorIs(t, err, ErrBlockedAddress)
	require.EqualValues(t, 1, hits.Load(), "only the allowed origin is hit; the redirect is refused")
}

// Environment proxies are ignored: the socket peer would be the proxy, whose
// view of the destination the dialer cannot validate. net/http caches proxy env
// per process, so the check runs in a fresh child.
func TestClientIgnoresEnvironmentProxy(t *testing.T) {
	if os.Getenv("OPENRAILS_HTTP_PROXY_TEST_CHILD") != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestClientIgnoresEnvironmentProxy$")
		cmd.Env = append(os.Environ(), "OPENRAILS_HTTP_PROXY_TEST_CHILD=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return
	}
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "proxied")
	}))
	defer proxy.Close()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, proxy.URL)
	}
	for _, key := range []string{"NO_PROXY", "no_proxy", "REQUEST_METHOD"} {
		t.Setenv(key, "")
	}
	client := Policy{Allow: AllowLoopback}.Client(time.Second)
	defer client.CloseIdleConnections()
	for _, scheme := range []string{"http", "https"} {
		resp, err := client.Get(scheme + "://outbound-policy.invalid/private")
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.Error(t, err, scheme)
	}
	require.Zero(t, calls.Load(), "the environment proxy must never be used")
}

func TestFailureDetailIsNotAnOracle(t *testing.T) {
	raw := fmt.Errorf(`Post "http://10.1.2.3:6379/": dial tcp 10.1.2.3:6379: connect: connection refused`)
	require.Equal(t, "delivery failed: the destination could not be reached", FailureDetail(raw))
	require.Equal(t, "destination address is not publicly routable", FailureDetail(fmt.Errorf("wrapped: %w", ErrBlockedAddress)))
	require.Equal(t, "url must be http or https", FailureDetail(ErrUnsupportedScheme))
	require.Empty(t, FailureDetail(nil))
}
