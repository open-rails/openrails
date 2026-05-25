package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/stretchr/testify/require"
)

func TestCaptchaStatusReportsGlobalChallenge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := captcha.NewChallengeStore(nil)
	require.NoError(t, store.MarkChallenged(context.Background(), "ip:203.0.113.50", time.Minute))

	s := &Server{
		cfg: &config.Config{Captcha: &config.CaptchaConfig{
			Enabled:   true,
			Provider:  config.CaptchaProviderTurnstile,
			SiteKey:   "site-key",
			SecretKey: "secret-key",
		}},
		captchaStore: store,
	}

	r := gin.New()
	r.GET("/v1/captcha/status", s.captchaStatusHandler)
	req := httptest.NewRequest(http.MethodGet, "/v1/captcha/status", nil)
	req.RemoteAddr = "203.0.113.50:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

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
	gin.SetMode(gin.TestMode)
	store := captcha.NewChallengeStore(nil)
	require.NoError(t, store.MarkChallenged(context.Background(), "user:user_1", time.Minute))

	s := &Server{
		cfg: &config.Config{Captcha: &config.CaptchaConfig{
			Enabled:   true,
			Provider:  config.CaptchaProviderTurnstile,
			SiteKey:   "site-key",
			SecretKey: "secret-key",
		}},
		captchaStore: store,
	}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		uc := authprovider.UserContext{UserID: "user_1"}
		c.Set("billing.user_context", uc)
		c.Request = c.Request.WithContext(authprovider.SetUserContext(c.Request.Context(), uc))
		c.Next()
	})
	r.GET("/v1/captcha/status", s.captchaStatusHandler)
	req := httptest.NewRequest(http.MethodGet, "/v1/captcha/status", nil)
	req.RemoteAddr = "203.0.113.51:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"required":true`)
}

func TestCaptchaStatusDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{cfg: &config.Config{Captcha: &config.CaptchaConfig{Enabled: false}}, captchaStore: captcha.NewChallengeStore(nil)}

	r := gin.New()
	r.GET("/v1/captcha/status", s.captchaStatusHandler)
	req := httptest.NewRequest(http.MethodGet, "/v1/captcha/status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"enabled": false, "required": false, "provider": "turnstile", "token_header": "X-Captcha-Token", "client_script_url": "/v1/captcha/client.js"}`, w.Body.String())
}

func TestCaptchaStatusEmbeddedRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{
		cfg: &config.Config{Captcha: &config.CaptchaConfig{
			Enabled:   true,
			Provider:  config.CaptchaProviderTurnstile,
			SiteKey:   "site-key",
			SecretKey: "secret-key",
		}},
		runtime:      &app.Runtime{},
		authProvider: passthroughAuthProvider{},
		captchaStore: captcha.NewChallengeStore(nil),
	}

	r := gin.New()
	s.registerUserRoutesAt(r, EmbeddedV1Prefix)
	req := httptest.NewRequest(http.MethodGet, "/billing/v1/captcha/status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"enabled":true`)
	require.Contains(t, w.Body.String(), `"client_script_url":"/billing/v1/captcha/client.js"`)
}

func TestCaptchaClientScriptIncludesProviderLoader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{cfg: &config.Config{Captcha: &config.CaptchaConfig{
		Enabled:   true,
		Provider:  config.CaptchaProviderHCaptcha,
		SiteKey:   "site-key",
		SecretKey: "secret-key",
	}}}

	r := gin.New()
	r.GET("/v1/captcha/client.js", s.captchaClientScriptHandler)
	req := httptest.NewRequest(http.MethodGet, "/v1/captcha/client.js", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/javascript; charset=utf-8", w.Header().Get("Content-Type"))
	require.Contains(t, w.Body.String(), "window.OpenRailsCaptcha")
	require.Contains(t, w.Body.String(), `provider: "hcaptcha"`)
	require.Contains(t, w.Body.String(), "https://js.hcaptcha.com/1/api.js?render=explicit")
	require.Contains(t, w.Body.String(), "site-key")
	require.NotContains(t, w.Body.String(), "secret-key")
}

func TestCaptchaClientScriptIncludesRecaptchaV3Execute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{cfg: &config.Config{Captcha: &config.CaptchaConfig{
		Enabled:   true,
		Provider:  config.CaptchaProviderRecaptchaV3,
		SiteKey:   "site-key",
		SecretKey: "secret-key",
		Action:    "billing_challenge",
	}}}

	r := gin.New()
	r.GET("/v1/captcha/client.js", s.captchaClientScriptHandler)
	req := httptest.NewRequest(http.MethodGet, "/v1/captcha/client.js", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `provider: "recaptcha-v3"`)
	require.Contains(t, w.Body.String(), "https://www.google.com/recaptcha/api.js?render=site-key")
	require.Contains(t, w.Body.String(), `action: "billing_challenge"`)
	require.Contains(t, w.Body.String(), "window.grecaptcha.execute(cfg.siteKey, { action: cfg.action })")
	require.NotContains(t, w.Body.String(), "secret-key")
}

func TestCaptchaClientScriptDisabledNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{cfg: &config.Config{Captcha: &config.CaptchaConfig{Enabled: false}}}

	r := gin.New()
	r.GET("/v1/captcha/client.js", s.captchaClientScriptHandler)
	req := httptest.NewRequest(http.MethodGet, "/v1/captcha/client.js", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "window.OpenRailsCaptcha")
	require.Contains(t, w.Body.String(), "enabled: false")
	require.NotContains(t, w.Body.String(), "site-key")
	require.NotContains(t, w.Body.String(), "secret-key")
}

type passthroughAuthProvider struct{}

func (passthroughAuthProvider) Required() gin.HandlerFunc { return passthrough }
func (passthroughAuthProvider) Optional() gin.HandlerFunc { return passthrough }

func passthrough(c *gin.Context) { c.Next() }
