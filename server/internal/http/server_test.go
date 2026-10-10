package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	auth "github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/staffperm"
)

func serve(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func captchaServer(provider string, store *captcha.ChallengeStore) *Server {
	cfg := &config.CaptchaConfig{}
	if provider != "" {
		cfg = &config.CaptchaConfig{Provider: provider, SiteKey: "site-key", SecretKey: "secret-key"}
	}
	return &Server{cfg: &config.Config{Captcha: cfg}, runtime: &app.Runtime{}, captchaStore: store}
}

// Captcha discovery is mounted under each surface prefix, reports a pending
// challenge for the caller's address, and never serves the secret.
func TestCaptchaDiscoveryRoutes(t *testing.T) {
	store := captcha.NewChallengeStore(abusestate.New(nil))
	store.MarkChallenged(context.Background(), "ip:203.0.113.50", time.Minute)

	status := func(s *Server, prefix, remote string) string {
		mux := http.NewServeMux()
		s.registerUserRoutesAt(mux, prefix)
		req := httptest.NewRequest(http.MethodGet, prefix+"/captcha/status", nil)
		req.RemoteAddr = remote
		w := serve(t, mux, req)
		require.Equal(t, http.StatusOK, w.Code)
		require.NotContains(t, w.Body.String(), "key")
		return w.Body.String()
	}
	body := func(enabled, required bool, prefix string) string {
		b, _ := json.Marshal(map[string]any{"enabled": enabled, "required": required, "provider": "turnstile", "token_header": "X-Captcha-Token", "client_script_url": prefix + "/captcha/client.js"})
		return string(b)
	}
	on := captchaServer(config.CaptchaProviderTurnstile, store)
	require.JSONEq(t, body(true, true, StandaloneV1Prefix), status(on, StandaloneV1Prefix, "203.0.113.50:1234"))
	require.JSONEq(t, body(true, false, StandaloneV1Prefix), status(on, StandaloneV1Prefix, "203.0.113.51:1234"))
	require.JSONEq(t, body(true, false, EmbeddedV1Prefix), status(on, EmbeddedV1Prefix, "203.0.113.51:1234"))
	require.JSONEq(t, body(false, false, StandaloneV1Prefix), status(captchaServer("", store), StandaloneV1Prefix, "203.0.113.50:1234"))

	for _, tc := range []struct {
		provider string
		want     []string
	}{
		{config.CaptchaProviderHCaptcha, []string{`provider: "hcaptcha"`, "https://js.hcaptcha.com/1/api.js?render=explicit", "site-key"}},
		{config.CaptchaProviderRecaptchaV3, []string{`provider: "recaptcha-v3"`, "https://www.google.com/recaptcha/api.js?render=site-key", `action: "billing_challenge"`, "window.grecaptcha.execute(cfg.siteKey, { action: cfg.action })"}},
		{"", []string{"enabled: false"}},
	} {
		mux := http.NewServeMux()
		captchaServer(tc.provider, nil).registerUserRoutesAt(mux, StandaloneV1Prefix)
		w := serve(t, mux, httptest.NewRequest(http.MethodGet, "/v1/captcha/client.js", nil))
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "application/javascript; charset=utf-8", w.Header().Get("Content-Type"))
		require.Contains(t, w.Body.String(), "window.OpenRailsCaptcha")
		for _, want := range tc.want {
			require.Contains(t, w.Body.String(), want, tc.provider)
		}
		require.NotContains(t, w.Body.String(), "secret-key")
	}
}

func TestStandaloneMetaRoutes(t *testing.T) {
	mux := http.NewServeMux()
	groups := config.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true}
	srv := &Server{groups: groups, permissions: permissionsFor(groups)}
	srv.registerStandaloneMetaRoutes(mux)
	require.True(t, srv.nativeBrowser["GET /v1/config"], "a browser reads the configuration from any origin")
	require.False(t, srv.nativeBrowser["GET /health/live"])
	get := func(path string) *httptest.ResponseRecorder {
		return serve(t, mux, httptest.NewRequest(http.MethodGet, path, nil))
	}
	require.JSONEq(t, `{"status":"ok"}`, get("/health/live").Body.String())
	for _, gone := range []string{"/", "/healthz", "/readyz", "/health/services"} {
		require.Equal(t, http.StatusNotFound, get(gone).Code, gone)
	}
	// Readiness detail is logged, never answered on the public route.
	ready := get("/health/ready?verbose=1")
	require.Equal(t, http.StatusServiceUnavailable, ready.Code)
	require.Contains(t, ready.Body.String(), `"service_unavailable"`)
	require.NotContains(t, ready.Body.String(), "postgres")

	// #623: the standalone server publishes the route groups it turns on; a
	// request that resolves no merchant has no payment setup.
	doc := get("/v1/config")
	require.Equal(t, http.StatusOK, doc.Code)
	var served billing.PublicConfig
	require.NoError(t, json.Unmarshal(doc.Body.Bytes(), &served), doc.Body.String())
	require.Equal(t, map[string]bool{"admin": true, "catalog": true, "merchant_config": true, "metrics": true, "app": true}, served.Capabilities.RouteGroups)
	require.Nil(t, served.Payment)
	require.Contains(t, doc.Body.String(), `"payment":null`)
	require.Equal(t, "public, max-age=300", doc.Header().Get("Cache-Control"))
	revalidate := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	revalidate.Header.Set("If-None-Match", doc.Header().Get("ETag"))
	require.Equal(t, http.StatusNotModified, serve(t, mux, revalidate).Code)

	// Merchant-config edits are advertised only where Vault holds it, with the
	// merchant-config group on.
	for _, kvMount := range []string{"", "kv"} {
		mux := http.NewServeMux()
		(&Server{cfg: &config.Config{}, groups: groups, permissions: permissionsFor(groups), runtime: &app.Runtime{
			Config: &config.Config{Vault: &config.VaultConfig{KVMount: kvMount}},
		}}).registerStandaloneMetaRoutes(mux)
		w := serve(t, mux, httptest.NewRequest(http.MethodGet, "/v1/config", nil))
		var doc billing.PublicConfig
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &doc))
		require.Equal(t, kvMount != "", doc.Capabilities.Features["merchant_config_edits"], kvMount)
	}
}

// challenging is an Authenticator that refuses every credential with its
// own challenge, and advertises the headers its credentials travel in.
type challenging struct{ origin string }

func (a *challenging) Authenticate(r *http.Request) (auth.Verified, error) {
	a.origin = r.Header.Get("Origin")
	return nil, &auth.Challenge{Err: auth.ErrSenderProofRequired, Header: http.Header{"Www-Authenticate": {`Proof error="use_nonce"`}, "Proof-Nonce": {"n1"}}}
}

func (*challenging) AllowedHeaders() []string { return []string{"Authorization", "Proof"} }
func (*challenging) ExposedHeaders() []string { return []string{"WWW-Authenticate", "Proof-Nonce"} }

// #469: the self-service surface is always mounted. Its CORS names the
// headers AuthKit's credentials travel in; a refused credential is a generic
// 401 whose challenge passes through unread.
func TestSelfServiceAuthentication(t *testing.T) {
	a := &challenging{}
	s := &Server{cfg: &config.Config{}, auth: a}
	mux := http.NewServeMux()
	s.registerSelfServiceRoutes(mux)
	ts := httptest.NewServer(s.wrapPublicHandler(mux))
	t.Cleanup(ts.Close)

	preflight := corsPreflight(t, ts, "/v1/me", "https://evil.example")
	require.Equal(t, http.StatusNoContent, preflight.StatusCode)
	require.Equal(t, "*", preflight.Header.Get("Access-Control-Allow-Origin"))
	require.Empty(t, preflight.Header.Get("Access-Control-Allow-Credentials"))
	require.Contains(t, preflight.Header.Get("Access-Control-Allow-Headers"), "Proof")
	require.Contains(t, preflight.Header.Get("Access-Control-Expose-Headers"), "Proof-Nonce")

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/me", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Authorization", "Bearer eyJ0eXAiOiJhdCtqd3QifQ.e30.sig")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Contains(t, string(body), "authentication_required")
	require.Equal(t, `Proof error="use_nonce"`, resp.Header.Get("WWW-Authenticate"))
	require.Equal(t, "n1", resp.Header.Get("Proof-Nonce"))
	require.Equal(t, "https://evil.example", a.origin)
}

func corsPreflight(t *testing.T, ts *httptest.Server, path, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodOptions, ts.URL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// #765: only browser-tier routes get the static wildcard grant (never with
// credentials); every other route gets no CORS headers from any origin.
func TestPermissiveCORSOnlyOnBrowserTier(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	mux.HandleFunc("GET /probe/browser", ok)
	s.recordBrowserRoute("GET /probe/browser")
	mux.HandleFunc("GET /probe/other", ok)
	ts := httptest.NewServer(s.wrapPublicHandler(mux))
	t.Cleanup(ts.Close)

	for _, origin := range []string{"https://storefront-a.example", "https://storefront-b.example"} {
		resp := corsPreflight(t, ts, "/probe/browser", origin)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		require.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
		require.Empty(t, resp.Header.Get("Access-Control-Allow-Credentials"))
		require.Empty(t, corsPreflight(t, ts, "/probe/other", origin).Header.Get("Access-Control-Allow-Origin"))
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/probe/other", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://storefront-a.example")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
}

// xs-007 row 37: a route that declares its budget owns its deadline; the
// server-wide WriteTimeout never cuts off a response to committed work.
func TestRouteBudgetOutlivesServerWriteTimeout(t *testing.T) {
	const serverWrite = 300 * time.Millisecond
	const work = 3 * serverWrite
	handler := func(budgeted bool) http.Handler {
		return middleware.RequestLogHTTP()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := httprequest.NewHTTP(w, r, nil)
			if budgeted {
				_, cancel := req.Budget(10 * work)
				defer cancel()
			}
			time.Sleep(work)
			req.SuccessJSON(map[string]any{"committed": true})
		}))
	}
	get := func(h http.Handler) (int, string, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		srv := &http.Server{Handler: h, WriteTimeout: serverWrite, ReadHeaderTimeout: time.Second}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}

	_, _, err := get(handler(false))
	require.Error(t, err, "negative control: the server clock cuts the response off")
	status, body, err := get(handler(true))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"committed":true`)

	rec := httptest.NewRecorder()
	req := httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil), nil)
	_, cancel := req.Budget(time.Second)
	cancel()
	req.SuccessJSON(map[string]any{"ok": true})
	require.Equal(t, http.StatusOK, rec.Code, "a writer without a connection deadline still serves")
}

// The server names its own merchant permission for every Routes.Permissions
// field, each one the merchant persona declares.
func TestMerchantPermissionsCoverEveryNeed(t *testing.T) {
	for _, n := range httproutes.AllNeeds() {
		perm := *merchantPermissions.Field(n)
		require.Contains(t, staffperm.Declared, perm, "Routes.Permissions.%s", n)
	}
	require.Len(t, staffperm.Declared, len(httproutes.AllNeeds()))
}
