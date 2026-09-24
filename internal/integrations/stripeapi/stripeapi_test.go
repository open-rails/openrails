package stripeapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/providerposture"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeStripe records every request that reaches the wire and answers the two
// posture reads.
type fakeStripe struct {
	*httptest.Server
	livemode atomic.Value
	account  string
	mu       sync.Mutex
	seen     []seenRequest
}

type seenRequest struct {
	Method, Path, Body string
	Query              url.Values
	Header             http.Header
}

func newFakeStripe(t *testing.T, livemode, account string) *fakeStripe {
	f := &fakeStripe{account: account}
	f.livemode.Store(livemode)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, seenRequest{Method: r.Method, Path: r.URL.Path, Body: string(body), Query: r.URL.Query(), Header: r.Header.Clone()})
		f.mu.Unlock()
		switch r.URL.Path {
		case "/v1/balance":
			_, _ = w.Write([]byte(`{"object":"balance","livemode":` + f.livemode.Load().(string) + `}`))
		case "/v1/account":
			_, _ = w.Write([]byte(`{"object":"account","id":"` + f.account + `"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"ch_1","body":"` + string(body) + `"}`))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeStripe) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.seen {
		if (method == "" || r.Method == method) && (path == "" || r.Path == path) {
			n++
		}
	}
	return n
}

func send(t *testing.T, c *http.Client, method, target, key string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, target, strings.NewReader("amount=1"))
	require.NoError(t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return req, err
}

func TestWriteModeGatesMutationsBeforeTheWire(t *testing.T) {
	f := newFakeStripe(t, "false", "acct")
	blocked := map[string]*http.Client{
		"readonly":         Client(&config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}, 0),
		"unset mode":       Client(&config.Config{}, 0),
		"nil config":       Client(nil, 0),
		"read-only client": ReadOnlyClient(0),
	}
	for name, c := range blocked {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			_, err := send(t, c, method, f.URL+"/v1/refunds", "")
			require.ErrorIs(t, errors.Join(errors.New("caller context"), err), ErrProviderReadOnly, "%s %s", name, method)
			require.ErrorContains(t, err, "mode=readonly")
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			_, err := send(t, c, method, f.URL+"/v1/balance", "")
			require.NoError(t, err, "%s %s: reads always pass", name, method)
		}
	}
	require.Zero(t, f.count(http.MethodPost, "")+f.count(http.MethodPut, "")+f.count(http.MethodPatch, "")+f.count(http.MethodDelete, ""))

	for name, cfg := range map[string]*config.Config{
		"full":            {ProviderWriteMode: config.ProviderWriteModeFull},
		"limited":         {ProviderWriteMode: config.ProviderWriteModeLimited},
		"sandbox full":    {ProviderWriteMode: config.ProviderWriteModeFull, TestMode: config.CredentialPostureSandbox},
		"sandbox limited": {ProviderWriteMode: config.ProviderWriteModeLimited, TestMode: config.CredentialPostureSandbox},
	} {
		_, err := send(t, Client(cfg, 0), http.MethodPost, f.URL+"/v1/customers", "sk_test_loopback")
		require.NoError(t, err, name)
	}
	require.Equal(t, 4, f.count(http.MethodPost, "/v1/customers"))
}

func TestGuardPinsVersionAndCarriesIdempotencyKey(t *testing.T) {
	f := newFakeStripe(t, "false", "acct")
	c := Client(&config.Config{ProviderWriteMode: config.ProviderWriteModeFull}, 0)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, err := send(t, c, method, f.URL+"/v1/charges", "")
		require.NoError(t, err)
		require.Empty(t, req.Header.Get(VersionHeader), "the caller's request is never mutated")
	}
	req, err := http.NewRequest(http.MethodPost, f.URL+"/v1/refunds", nil)
	require.NoError(t, err)
	req.Header.Set(VersionHeader, "2099-01-01.custom")
	SetIdempotencyKey(req, "intent-key")
	SetIdempotencyKey(req, "")
	SetIdempotencyKey(nil, "ignored")
	resp, err := c.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.seen, 3)
	require.Equal(t, APIVersion, f.seen[0].Header.Get(VersionHeader))
	require.Equal(t, APIVersion, f.seen[1].Header.Get(VersionHeader))
	require.Equal(t, "2099-01-01.custom", f.seen[2].Header.Get(VersionHeader), "a caller-set version wins")
	require.Equal(t, "intent-key", f.seen[2].Header.Get(IdempotencyKeyHeader))
}

func TestTimeoutDefaults(t *testing.T) {
	require.Equal(t, DefaultTimeout, Client(nil, 0).Timeout)
	require.Equal(t, 30*time.Second, Client(nil, 30*time.Second).Timeout)
	require.Equal(t, DefaultTimeout, ReadOnlyClient(-1).Timeout)
}

// An injected transport sits UNDER the guard: readonly still refuses writes
// before the host transport, the version is still pinned, and sandbox posture
// still refuses a live key.
func TestFactoryInstallsUnderTheGuard(t *testing.T) {
	var reached atomic.Int64
	factory := NewFactory(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		reached.Add(1)
		require.Equal(t, APIVersion, req.Header.Get(VersionHeader))
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}))
	full := factory.Client(&config.Config{ProviderWriteMode: config.ProviderWriteModeFull}, 0)
	sandbox := factory.Client(&config.Config{ProviderWriteMode: config.ProviderWriteModeFull, TestMode: config.CredentialPostureSandbox}, 0)

	_, err := send(t, full, http.MethodPost, APIBase+"/v1/products", "sk_live_x")
	require.NoError(t, err)
	_, err = send(t, factory.ReadOnlyClient(0), http.MethodPost, APIBase+"/v1/products", "sk_test_x")
	require.ErrorIs(t, err, ErrProviderReadOnly)
	_, err = send(t, sandbox, http.MethodPost, APIBase+"/v1/products", "sk_live_x")
	require.ErrorIs(t, err, providerposture.ErrDisarmed)
	_, err = send(t, sandbox, http.MethodPost, APIBase+"/v1/products", "sk_test_x")
	require.NoError(t, err)
	require.EqualValues(t, 2, reached.Load())

	require.Equal(t, providerposture.Live, factory.VerifyPosture(context.Background(), "rk_live_x", "").Verdict)
	require.Equal(t, providerposture.Simulated, factory.VerifyPosture(context.Background(), "sk_test_x", "").Verdict)
	verdict, err := factory.PostureCheckFor("sk_live_x", "")(context.Background())
	require.Equal(t, providerposture.Live, verdict)
	require.Error(t, err)
}

// sandboxClient routes api.stripe.com to the fake without the fixture
// exemption, so the real posture gate runs.
func sandboxClient(f *fakeStripe) *http.Client {
	return &http.Client{Transport: &guardTransport{sandbox: true, base: HostRewriteTransport(f.URL)}}
}

func TestSandboxPostureVerifiesEachKeyOnceAndRefusesLive(t *testing.T) {
	f := newFakeStripe(t, "false", "acct_1")
	c := sandboxClient(f)
	key := "sk_test_" + uuid.NewString()
	for range 3 {
		_, err := send(t, c, http.MethodPost, APIBase+"/v1/charges?expand=x", key)
		require.NoError(t, err)
	}
	require.Equal(t, 1, f.count(http.MethodGet, "/v1/balance"), "one read-only verification per key")
	require.Equal(t, 3, f.count(http.MethodPost, "/v1/charges"))
	f.mu.Lock()
	balance, charge := f.seen[0], f.seen[len(f.seen)-1]
	f.mu.Unlock()
	require.Equal(t, "Bearer "+key, balance.Header.Get("Authorization"))
	require.Equal(t, APIVersion, balance.Header.Get(VersionHeader))
	require.Equal(t, "x", charge.Query.Get("expand"), "the host rewrite preserves path, query and body")
	require.Equal(t, "amount=1", charge.Body)

	_, err := send(t, c, http.MethodGet, APIBase+"/v1/customers", "sk_live_"+uuid.NewString())
	require.NoError(t, err, "reads are never posture-gated")
	for name, key := range map[string]string{"live key": "sk_live_" + uuid.NewString(), "restricted live key": "rk_live_" + uuid.NewString(), "unprefixed key": uuid.NewString()} {
		_, err := send(t, c, http.MethodPost, APIBase+"/v1/charges", key)
		require.ErrorIs(t, err, providerposture.ErrDisarmed, name)
	}
	require.Equal(t, 1, f.count(http.MethodGet, "/v1/balance"), "a key without a test prefix is refused without a provider read")

	f.livemode.Store("true")
	_, err = send(t, c, http.MethodPost, APIBase+"/v1/charges", "rk_test_"+uuid.NewString())
	require.ErrorIs(t, err, providerposture.ErrDisarmed, "a test-prefixed key on a livemode account is refused")
	require.Equal(t, 3, f.count(http.MethodPost, "/v1/charges"))
}

func TestPostureCheckBindsDeclaredAndConnectedAccount(t *testing.T) {
	f := newFakeStripe(t, "false", "acct_real")
	key := "sk_test_" + uuid.NewString()
	status := providerposture.Process().Verify(context.Background(), PostureKey(key, APIBase, ""), PostureCheck(HostRewriteTransport(f.URL), key, "", "acct_declared"))
	require.Equal(t, providerposture.Mismatched, status.Verdict)
	_, err := send(t, sandboxClient(f), http.MethodPost, APIBase+"/v1/charges", key)
	require.ErrorIs(t, err, providerposture.ErrDisarmed, "the transport gate honors the startup verdict")

	verdict, err := PostureCheck(HostRewriteTransport(f.URL), key, "acct_connected", "acct_real")(context.Background())
	require.NoError(t, err)
	require.Equal(t, providerposture.Simulated, verdict)
	f.mu.Lock()
	last := f.seen[len(f.seen)-1]
	f.mu.Unlock()
	require.Equal(t, "acct_connected", last.Header.Get("Stripe-Account"))

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"object":"balance"}`)) }))
	defer bad.Close()
	verdict, err = PostureCheck(HostRewriteTransport(bad.URL), key, "", "")(context.Background())
	require.Equal(t, providerposture.Unknown, verdict, "a balance without livemode proves nothing")
	require.Error(t, err)
}

func TestLoopbackFixtureStillRefusesLiveKeys(t *testing.T) {
	f := newFakeStripe(t, "true", "acct_1")
	c := &http.Client{Transport: &guardTransport{sandbox: true}}
	_, err := send(t, c, http.MethodPost, f.URL+"/v1/charges", "sk_test_fixture")
	require.NoError(t, err)
	_, err = send(t, c, http.MethodPost, f.URL+"/v1/charges", "sk_live_fixture")
	require.ErrorIs(t, err, providerposture.ErrDisarmed)
	req, err := http.NewRequest(http.MethodPost, f.URL+"/v1/charges", nil)
	require.NoError(t, err)
	req.SetBasicAuth("sk_live_fixture", "")
	_, err = c.Do(req) //nolint:bodyclose // refused before a response exists
	require.ErrorIs(t, err, providerposture.ErrDisarmed, "a basic-auth live key is recognized too")
	require.Zero(t, f.count(http.MethodGet, "/v1/balance"), "loopback fixtures never run a provider read")
}
