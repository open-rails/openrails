package stripeapi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

// newCountingServer returns an httptest server that counts how many requests
// actually reach it. Blocked writes must never increment the counter.
func newCountingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestReadOnlyBlocksWritesLocally(t *testing.T) {
	srv, hits := newCountingServer(t)
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}
	client := Client(cfg, 0)

	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch} {
		req, err := http.NewRequest(method, srv.URL+"/v1/refunds", strings.NewReader("charge=ch_123"))
		require.NoError(t, err)
		resp, err := client.Do(req) //nolint:bodyclose // resp is nil on error
		require.Error(t, err, method)
		require.Nil(t, resp, method)
		// http.Client wraps transport errors in *url.Error; the sentinel must
		// still be reachable via errors.Is.
		require.ErrorIs(t, err, ErrProviderReadOnly, method)
		require.Contains(t, err.Error(), "mode=readonly", method)
	}
	require.Equal(t, int64(0), hits.Load(), "blocked writes must never reach the wire")
}

func TestReadOnlyAllowsGet(t *testing.T) {
	srv, hits := newCountingServer(t)
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}
	client := Client(cfg, 0)

	resp, err := client.Get(srv.URL + "/v1/balance")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.JSONEq(t, `{"ok":true}`, string(body))
	require.Equal(t, int64(1), hits.Load())
}

func TestNonReadOnlyAllowsWrites(t *testing.T) {
	srv, hits := newCountingServer(t)
	for _, cfg := range []*config.Config{
		nil, // nil = tests/full (documented Client contract)
		{ProviderWriteMode: config.ProviderWriteModeFull, TestMode: config.CredentialPostureSandbox}, // sandbox creds, full behavior
		{ProviderWriteMode: config.ProviderWriteModeFull},
		{ProviderWriteMode: config.ProviderWriteModeLimited}, // limited gates live at the dispatcher/worker layer, not the wire
	} {
		client := Client(cfg, 0)
		resp, err := client.Post(srv.URL+"/v1/customers", "application/x-www-form-urlencoded", strings.NewReader("email=a@b.c"))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
	require.Equal(t, int64(4), hits.Load())
}

// provider_write_mode FAIL-CLOSES (2026-07-02): an EMPTY config (mode never
// declared) blocks writes at the wire exactly like an explicit readonly.
func TestUnsetModeFailsClosedAtTheWire(t *testing.T) {
	srv, hits := newCountingServer(t)
	client := Client(&config.Config{}, 0)
	resp, err := client.Post(srv.URL+"/v1/customers", "application/x-www-form-urlencoded", strings.NewReader("email=a@b.c")) //nolint:bodyclose // resp is nil on error
	require.Error(t, err)
	require.Nil(t, resp)
	require.ErrorIs(t, err, ErrProviderReadOnly)
	require.Equal(t, int64(0), hits.Load(), "unset mode must never reach the wire")
}

func TestReadOnlyClientAlwaysBlocksWrites(t *testing.T) {
	srv, hits := newCountingServer(t)
	client := ReadOnlyClient(0)

	resp, err := client.Post(srv.URL+"/v1/products", "application/x-www-form-urlencoded", strings.NewReader("name=x"))
	require.Error(t, err)
	require.Nil(t, resp)
	require.ErrorIs(t, err, ErrProviderReadOnly)
	require.Equal(t, int64(0), hits.Load())

	// reads still work
	getResp, err := client.Get(srv.URL + "/v1/balance")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	require.NoError(t, getResp.Body.Close())
	require.Equal(t, int64(1), hits.Load())
}

func TestTimeoutDefaults(t *testing.T) {
	require.Equal(t, DefaultTimeout, Client(nil, 0).Timeout)
	require.Equal(t, 30*time.Second, Client(nil, 30*time.Second).Timeout)
	require.Equal(t, DefaultTimeout, ReadOnlyClient(-1).Timeout)
}

// TestPinsStripeVersionHeader proves every outbound request carries the pinned
// Stripe-Version (#587), on both reads and writes, and that a caller-set version
// is left untouched.
func TestPinsStripeVersionHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(VersionHeader)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	client := Client(nil, 0)

	// GET pins the version.
	resp, err := client.Get(srv.URL + "/v1/balance")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, APIVersion, got)

	// POST pins the version.
	got = ""
	resp, err = client.Post(srv.URL+"/v1/customers", "application/x-www-form-urlencoded", strings.NewReader("email=a@b.c"))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, APIVersion, got)

	// A caller-set version is preserved (override wins).
	got = ""
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/balance", nil)
	require.NoError(t, err)
	req.Header.Set(VersionHeader, "2099-01-01.custom")
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, "2099-01-01.custom", got)
}

// TestReadonlyModeBlocksServiceWrite proves the gate composes with a real
// errors.Is check at a caller boundary, the way service code consumes it.
func TestSentinelSurvivesWrapping(t *testing.T) {
	client := ReadOnlyClient(0)
	req, err := http.NewRequest(http.MethodPost, "https://api.stripe.com/v1/charges", nil)
	require.NoError(t, err)
	_, doErr := client.Do(req) //nolint:bodyclose // resp is nil on error
	require.Error(t, doErr)
	wrapped := errors.Join(errors.New("stripe charge failed"), doErr)
	require.ErrorIs(t, wrapped, ErrProviderReadOnly)
}
