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
	"github.com/stretchr/testify/require"
)

func TestCaptchaStatusReportsGlobalChallenge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := captcha.NewChallengeStore(nil)
	require.NoError(t, store.MarkChallenged(context.Background(), "203.0.113.50", time.Minute))

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
		"site_key": "site-key",
		"token_header": "X-Captcha-Token"
	}`, w.Body.String())
	require.NotContains(t, w.Body.String(), "secret-key")
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
	require.JSONEq(t, `{"enabled": false, "required": false, "token_header": "X-Captcha-Token"}`, w.Body.String())
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
}

type passthroughAuthProvider struct{}

func (passthroughAuthProvider) Required() gin.HandlerFunc { return passthrough }
func (passthroughAuthProvider) Optional() gin.HandlerFunc { return passthrough }

func passthrough(c *gin.Context) { c.Next() }
