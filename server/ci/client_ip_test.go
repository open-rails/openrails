//go:build e2e && integration

package ci_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
)

// The proxy and a peer outside it. The kernel routes all of 127/8 to
// loopback, so each request's peer is its real socket address.
const proxyPeer, outsiderPeer = "127.0.0.1", "127.0.0.2"

// A server behind a proxy declares it once, on Engine: the engine's rate
// limits, AuthKit's and the request log all name the client the proxy
// forwards, and a peer outside it is the client whatever it forwards.
func TestServerNamesTheClientBehindItsProxy(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.Engine.TrustedProxies = []string{proxyPeer + "/32"}
		cfg.Engine.RateLimits = &openrails.RateLimitsConfig{"purchase": {RequestsPerMinute: 2}}
		// No client-IP posture of its own: AuthKit takes the engine's.
		cfg.Auth.HTTP = &authkit.HTTPConfig{RateLimits: map[string]authkit.RateLimit{"password_login": {Limit: 2, Window: time.Hour}}}
	})
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	front := behindProxy(t, handler)

	pay := func() string { return "/v1/me/orders/" + billing.OrderID(uuid.New()).String() + "/pay" }
	front.limitsTheClient(t, "engine purchase", pay, `{}`)
	login := func() string { return "/" + f.schema + "/v1/password/login" }
	front.limitsTheClient(t, "AuthKit password login", login, `{"identifier":"nobody@e2e.test","password":"wrong-password"}`)
}

// An embedded engine's payment page logs the client its proxy forwards.
func TestEmbeddedPaymentLogsTheClientBehindItsProxy(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.TrustedProxies = []string{proxyPeer + "/32"}
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"proxied-purchase": {DisplayName: "Proxied purchase"}}
	bill, _ := f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	_, handler, err := bill.PaymentPage("https://pay.e2e.test", paymentPage)
	require.NoError(t, err)
	front := behindProxy(t, handler)

	page := "/pay/" + billing.OrderID(uuid.New()).String()
	status, ip := front.send(t, front.proxy, http.MethodGet, page, "", "198.51.100.1")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "198.51.100.1", ip, "the client behind the proxy")
	status, ip = front.send(t, front.outsider, http.MethodGet, page, "", "198.51.100.1")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, outsiderPeer, ip, "an untrusted peer, whatever it forwards")
}

// proxyFront serves a handler over HTTP to the proxy and to an outsider.
type proxyFront struct {
	url             string
	proxy, outsider *http.Client
	logs            *logtest.Hook
}

func behindProxy(t *testing.T, handler http.Handler) *proxyFront {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	logs := logtest.NewGlobal()
	t.Cleanup(logs.Reset)
	return &proxyFront{url: ts.URL, proxy: dialFrom(t, proxyPeer), outsider: dialFrom(t, outsiderPeer), logs: logs}
}

// dialFrom is a client whose connections leave from addr.
func dialFrom(t *testing.T, addr string) *http.Client {
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(addr)}}
	transport := &http.Transport{DialContext: dialer.DialContext}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

// send is one request from client forwarding forwardedFor: its status and the
// ip its request log line names.
func (p *proxyFront) send(t *testing.T, client *http.Client, method, path, body, forwardedFor string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, p.url+path, strings.NewReader(body))
	require.NoError(t, err)
	id := uuid.NewString()
	req.Header.Set("X-Request-ID", id)
	req.Header.Set("X-Forwarded-For", forwardedFor)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, res.Body)
	require.NoError(t, res.Body.Close())
	var ip any
	require.Eventually(t, func() bool {
		for _, entry := range p.logs.AllEntries() {
			if entry.Message == method+" "+path && entry.Data["request_id"] == id {
				ip = entry.Data["ip"]
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "%s %s is logged", method, path)
	return res.StatusCode, fmt.Sprint(ip)
}

// limitsTheClient spends a budget of two on path from a client behind the
// proxy, then from an outsider forging X-Forwarded-For. Every request is
// refused as unauthenticated until its client's budget is spent.
func (p *proxyFront) limitsTheClient(t *testing.T, name string, path func() string, body string) {
	t.Helper()
	// Windows are clock-aligned: keep the burst inside one.
	if left := time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)); left < 10*time.Second {
		time.Sleep(left + 100*time.Millisecond)
	}
	expect := func(from *http.Client, forwardedFor string, status int, client, why string) {
		t.Helper()
		gotStatus, gotIP := p.send(t, from, http.MethodPost, path(), body, forwardedFor)
		require.Equal(t, status, gotStatus, "%s: %s", name, why)
		require.Equal(t, client, gotIP, "%s: the request log names the client: %s", name, why)
	}
	for range 2 {
		expect(p.proxy, "198.51.100.1", http.StatusUnauthorized, "198.51.100.1", "within the forwarded client's budget")
	}
	expect(p.proxy, "198.51.100.1", http.StatusTooManyRequests, "198.51.100.1", "the forwarded client spent its budget")
	expect(p.proxy, "198.51.100.2", http.StatusUnauthorized, "198.51.100.2", "another client behind the proxy has its own budget")
	for i := range 2 {
		expect(p.outsider, fmt.Sprintf("198.51.100.%d", 10+i), http.StatusUnauthorized, outsiderPeer, "an untrusted peer is its own client")
	}
	expect(p.outsider, "198.51.100.12", http.StatusTooManyRequests, outsiderPeer, "a forged X-Forwarded-For buys no fresh budget")
}
