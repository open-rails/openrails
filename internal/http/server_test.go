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

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	auth "github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/pkg/billingauth"
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
// challenge for the caller's address or user, and never serves the secret.
func TestCaptchaDiscoveryRoutes(t *testing.T) {
	store := captcha.NewChallengeStore(nil)
	require.NoError(t, store.MarkChallenged(context.Background(), "ip:203.0.113.50", time.Minute))
	require.NoError(t, store.MarkChallenged(context.Background(), "user:user_1", time.Minute))

	status := func(s *Server, prefix, remote, user string) string {
		mux := http.NewServeMux()
		s.registerUserRoutesAt(mux, prefix)
		req := httptest.NewRequest(http.MethodGet, prefix+"/captcha/status", nil)
		req.RemoteAddr = remote
		if user != "" {
			req = req.WithContext(billingauth.SetUserContext(req.Context(), billingauth.UserContext{UserID: user}))
		}
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
	require.JSONEq(t, body(true, true, StandaloneV1Prefix), status(on, StandaloneV1Prefix, "203.0.113.50:1234", ""))
	require.JSONEq(t, body(true, true, StandaloneV1Prefix), status(on, StandaloneV1Prefix, "203.0.113.51:1234", "user_1"))
	require.JSONEq(t, body(true, false, StandaloneV1Prefix), status(on, StandaloneV1Prefix, "203.0.113.51:1234", "user_2"))
	require.JSONEq(t, body(true, false, EmbeddedV1Prefix), status(on, EmbeddedV1Prefix, "203.0.113.51:1234", ""))
	require.JSONEq(t, body(false, false, StandaloneV1Prefix), status(captchaServer("", store), StandaloneV1Prefix, "203.0.113.50:1234", ""))

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
	(&Server{}).registerStandaloneMetaRoutes(mux)
	get := func(path string) *httptest.ResponseRecorder {
		return serve(t, mux, httptest.NewRequest(http.MethodGet, path, nil))
	}
	require.Equal(t, http.StatusOK, get("/").Code)
	require.Contains(t, get("/").Body.String(), `"service"`)
	require.Equal(t, http.StatusNotFound, get("/nope").Code, "the root banner is exact-match")
	require.Equal(t, http.StatusNotFound, get("/health/services").Code)
	ready := get("/health/ready?verbose=1")
	require.Equal(t, http.StatusServiceUnavailable, ready.Code)
	require.Contains(t, ready.Body.String(), `"dependencies"`)

	// #623: configuration routes are advertised only when explicitly published.
	caps := get("/v1/capabilities")
	require.Equal(t, http.StatusOK, caps.Code)
	var groups struct {
		RouteGroups map[string]bool `json:"route_groups"`
	}
	require.NoError(t, json.Unmarshal(caps.Body.Bytes(), &groups), caps.Body.String())
	for _, rs := range embedhttp.StandaloneDefaultRouteSets {
		require.Equal(t, rs != embedhttp.RouteSetMerchantConfig, groups.RouteGroups[string(rs)], rs)
	}

	// Provider credential writes are advertised only for a writable DB backend.
	for _, source := range []string{config.SecretBackendSnapshot, config.SecretBackendDB} {
		for _, writable := range []bool{false, true} {
			mux := http.NewServeMux()
			(&Server{cfg: &config.Config{MerchantConfigHTTP: true}, runtime: &app.Runtime{
				Config:            &config.Config{SecretBackend: source},
				RouteCapabilities: &routesurface.RuntimeCapabilities{SecretWrite: writable},
			}}).registerStandaloneMetaRoutes(mux)
			w := serve(t, mux, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
			var caps embedhttp.Capabilities
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &caps))
			require.Equal(t, source == config.SecretBackendDB && writable, caps.Features["provider_credential_writes"], "%s writable=%v", source, writable)
		}
	}
}

type hostAuthenticator struct{}

func (hostAuthenticator) AuthenticateDelegated(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return &billingauth.DelegatedPrincipal{MerchantID: uuid.NewString(), SubjectID: "11111111-1111-1111-1111-111111111111"}, nil
}

type proofRejectingResolver struct{ origin string }

func (r *proofRejectingResolver) ResolveDelegated(req *http.Request) (*controlplane.ResolvedDelegated, error) {
	r.origin = req.Header.Get("Origin")
	return nil, auth.ErrSenderProofRequired
}

// #339/#469: the self-service surface is always mounted; a host authenticator
// admits its principal without a control plane, while the control-plane path
// demands sender proof even from a CORS-allowed origin.
func TestSelfServiceAuthentication(t *testing.T) {
	hosted := &Server{cfg: &config.Config{}, delegatedAuthenticator: hostAuthenticator{}}
	mux := http.NewServeMux()
	hosted.registerSelfServiceRoutes(mux)
	w := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }() // the handler may panic past auth on the bare runtime
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/me/checkout", nil))
	}()
	require.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}, w.Code, w.Body.String())

	resolver := &proofRejectingResolver{}
	s := &Server{cfg: &config.Config{}, delegatedResolver: resolver}
	mux = http.NewServeMux()
	s.registerSelfServiceRoutes(mux)
	ts := httptest.NewServer(s.wrapPublicHandler(mux))
	t.Cleanup(ts.Close)

	preflight := corsPreflight(t, ts, "/v1/me/status", "https://evil.example")
	require.Equal(t, http.StatusNoContent, preflight.StatusCode)
	require.Equal(t, "*", preflight.Header.Get("Access-Control-Allow-Origin"))
	require.Empty(t, preflight.Header.Get("Access-Control-Allow-Credentials"))

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/me/status", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Authorization", "Bearer delegated.jwt.token")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Contains(t, string(body), "sender_proof_required")
	require.Equal(t, "https://evil.example", resolver.origin)
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
