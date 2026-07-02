package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func captchaMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	s.registerUserRoutesAt(mux, StandaloneV1Prefix)
	return mux
}

func TestCaptchaStatusReportsGlobalChallenge(t *testing.T) {
	store := captcha.NewChallengeStore(nil)
	require.NoError(t, store.MarkChallenged(context.Background(), "ip:203.0.113.50", time.Minute))

	s := &Server{
		cfg: &config.Config{Captcha: &config.CaptchaConfig{
			Provider:  config.CaptchaProviderTurnstile,
			SiteKey:   "site-key",
			SecretKey: "secret-key",
		}},
		runtime:      &app.Runtime{},
		captchaStore: store,
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/captcha/status", nil)
	req.RemoteAddr = "203.0.113.50:1234"
	w := httptest.NewRecorder()
	captchaMux(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{
		"enabled": true,
		"required": true,
		"provider": "turnstile",
		"token_header": "X-Captcha-Token",
		"client_script_url": "/v1/captcha/client.js"
	}`, w.Body.String())
	require.NotContains(t, w.Body.String(), "site-key")
	require.NotContains(t, w.Body.String(), "secret-key")
}

func TestCaptchaStatusReportsUserChallenge(t *testing.T) {
	store := captcha.NewChallengeStore(nil)
	require.NoError(t, store.MarkChallenged(context.Background(), "user:user_1", time.Minute))

	s := &Server{
		cfg: &config.Config{Captcha: &config.CaptchaConfig{
			Provider:  config.CaptchaProviderTurnstile,
			SiteKey:   "site-key",
			SecretKey: "secret-key",
		}},
		runtime:      &app.Runtime{},
		captchaStore: store,
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/captcha/status", nil)
	req.RemoteAddr = "203.0.113.51:1234"
	req = req.WithContext(billingauth.SetUserContext(req.Context(), billingauth.UserContext{UserID: "user_1"}))
	w := httptest.NewRecorder()
	captchaMux(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"required":true`)
}

func TestCaptchaStatusDisabled(t *testing.T) {
	s := &Server{
		cfg:          &config.Config{Captcha: &config.CaptchaConfig{}},
		runtime:      &app.Runtime{},
		captchaStore: captcha.NewChallengeStore(nil),
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/captcha/status", nil)
	w := httptest.NewRecorder()
	captchaMux(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"enabled": false, "required": false, "provider": "turnstile", "token_header": "X-Captcha-Token", "client_script_url": "/v1/captcha/client.js"}`, w.Body.String())
}

func TestCaptchaStatusEmbeddedRoute(t *testing.T) {
	s := &Server{
		cfg: &config.Config{Captcha: &config.CaptchaConfig{
			Provider:  config.CaptchaProviderTurnstile,
			SiteKey:   "site-key",
			SecretKey: "secret-key",
		}},
		runtime:      &app.Runtime{},
		captchaStore: captcha.NewChallengeStore(nil),
	}

	mux := http.NewServeMux()
	s.registerUserRoutesAt(mux, EmbeddedV1Prefix)
	req := httptest.NewRequest(http.MethodGet, "/billing/v1/captcha/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"enabled":true`)
	require.Contains(t, w.Body.String(), `"client_script_url":"/billing/v1/captcha/client.js"`)
}

func TestCaptchaClientScriptIncludesProviderLoader(t *testing.T) {
	s := &Server{cfg: &config.Config{Captcha: &config.CaptchaConfig{
		Provider:  config.CaptchaProviderHCaptcha,
		SiteKey:   "site-key",
		SecretKey: "secret-key",
	}}, runtime: &app.Runtime{}}

	req := httptest.NewRequest(http.MethodGet, "/v1/captcha/client.js", nil)
	w := httptest.NewRecorder()
	captchaMux(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/javascript; charset=utf-8", w.Header().Get("Content-Type"))
	require.Contains(t, w.Body.String(), "window.OpenRailsCaptcha")
	require.Contains(t, w.Body.String(), `provider: "hcaptcha"`)
	require.Contains(t, w.Body.String(), "https://js.hcaptcha.com/1/api.js?render=explicit")
	require.Contains(t, w.Body.String(), "site-key")
	require.NotContains(t, w.Body.String(), "secret-key")
}

func TestCaptchaClientScriptIncludesRecaptchaV3Execute(t *testing.T) {
	s := &Server{cfg: &config.Config{Captcha: &config.CaptchaConfig{
		Provider:  config.CaptchaProviderRecaptchaV3,
		SiteKey:   "site-key",
		SecretKey: "secret-key",
	}}, runtime: &app.Runtime{}}

	req := httptest.NewRequest(http.MethodGet, "/v1/captcha/client.js", nil)
	w := httptest.NewRecorder()
	captchaMux(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `provider: "recaptcha-v3"`)
	require.Contains(t, w.Body.String(), "https://www.google.com/recaptcha/api.js?render=site-key")
	require.Contains(t, w.Body.String(), `action: "billing_challenge"`)
	require.Contains(t, w.Body.String(), "window.grecaptcha.execute(cfg.siteKey, { action: cfg.action })")
	require.NotContains(t, w.Body.String(), "secret-key")
}

func TestCaptchaClientScriptDisabledNoop(t *testing.T) {
	s := &Server{cfg: &config.Config{Captcha: &config.CaptchaConfig{}}, runtime: &app.Runtime{}}

	req := httptest.NewRequest(http.MethodGet, "/v1/captcha/client.js", nil)
	w := httptest.NewRecorder()
	captchaMux(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "window.OpenRailsCaptcha")
	require.Contains(t, w.Body.String(), "enabled: false")
	require.NotContains(t, w.Body.String(), "site-key")
	require.NotContains(t, w.Body.String(), "secret-key")
}
