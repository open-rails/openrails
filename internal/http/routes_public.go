package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
	captchaembed "github.com/open-rails/openrails/internal/captcha/embed"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	"github.com/open-rails/openrails/internal/http/router/ginrouter"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

func (s *Server) registerUserRoutesAt(e *gin.Engine, apiPrefix string) {
	api := e.Group(apiPrefix)
	api.GET("/captcha/status", s.captchaStatusHandler)
	api.GET("/captcha/client.js", s.captchaClientScriptHandler)
	httproutes.RegisterUserRoutes(ginrouter.New(api, s.runtime), s.runtime, httproutes.Options{
		Authenticator: s.embeddedAuthenticator(),
	})
}

func (s *Server) captchaStatusHandler(c *gin.Context) {
	var cfg *config.CaptchaConfig
	if s != nil && s.cfg != nil {
		cfg = s.cfg.Captcha
	}
	resp := gin.H{
		"enabled":           cfg != nil && cfg.Enabled,
		"required":          false,
		"token_header":      captcha.TokenHeader,
		"client_script_url": captchaClientScriptURL(c),
	}
	if cfg != nil {
		resp["provider"] = cfg.EffectiveProvider()
	}
	if cfg == nil || !cfg.Enabled || s.captchaStore == nil {
		c.JSON(http.StatusOK, resp)
		return
	}

	required := false
	for _, subjectKey := range ginmw.RateLimitSubjectKeys(c) {
		challenged, err := s.captchaStore.IsChallenged(c.Request.Context(), subjectKey)
		if err != nil {
			continue
		}
		if challenged {
			required = true
			break
		}
	}

	resp["required"] = required
	c.JSON(http.StatusOK, resp)
}

func (s *Server) captchaClientScriptHandler(c *gin.Context) {
	var cfg *config.CaptchaConfig
	if s != nil && s.cfg != nil {
		cfg = s.cfg.Captcha
	}
	script := buildCaptchaClientScript(cfg)
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/javascript; charset=utf-8", []byte(script))
}

func captchaClientScriptURL(c *gin.Context) string {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return "/v1/captcha/client.js"
	}
	path := c.Request.URL.Path
	if strings.HasSuffix(path, "/status") {
		return strings.TrimSuffix(path, "/status") + "/client.js"
	}
	return "/v1/captcha/client.js"
}

func buildCaptchaClientScript(cfg *config.CaptchaConfig) string {
	enabled := cfg != nil && cfg.Enabled
	provider := ""
	siteKey := ""
	scriptURL := ""
	action := ""
	if cfg != nil {
		provider = cfg.EffectiveProvider()
		action = cfg.EffectiveAction()
	}
	if enabled {
		siteKey = strings.TrimSpace(cfg.SiteKey)
		scriptURL = cfg.EffectiveScriptURL()
	}

	return strings.NewReplacer(
		"__OPENRAILS_CAPTCHA_ENABLED__", strconv.FormatBool(enabled),
		"__OPENRAILS_CAPTCHA_PROVIDER__", jsonLiteral(provider),
		"__OPENRAILS_CAPTCHA_SITE_KEY__", jsonLiteral(siteKey),
		"__OPENRAILS_CAPTCHA_SCRIPT_URL__", jsonLiteral(scriptURL),
		"__OPENRAILS_CAPTCHA_ACTION__", jsonLiteral(action),
	).Replace(captchaembed.ClientScriptTemplate)
}

func jsonLiteral(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "\"\""
	}
	return string(raw)
}

func (s *Server) registerUserRoutes(e *gin.Engine) {
	s.registerUserRoutesAt(e, StandaloneV1Prefix)
}

func (s *Server) registerWebhookRoutesAt(e *gin.Engine, apiPrefix string) {
	api := e.Group(apiPrefix)
	webhooks := api.Group("/webhooks")
	httproutes.RegisterWebhookRoutes(ginrouter.New(webhooks, s.runtime), s.runtime)
}

func (s *Server) registerWebhookRoutes(e *gin.Engine) {
	s.registerWebhookRoutesAt(e, StandaloneV1Prefix)
}

// registerStandaloneMetaRoutes registers banner/health endpoints that are appropriate for the
// standalone billing service, but should not be forced onto embedded hosts.
func (s *Server) registerStandaloneMetaRoutes(e *gin.Engine) {
	// Root: simple JSON banner for API servers
	e.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"service":   "billing",
			"status":    "ok",
			"endpoints": []string{"/health/live", "/health/ready", StandaloneV1Prefix},
		})
	})

	e.GET("/health/live", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "billing"})
	})

	e.GET("/health/ready", s.readyHandler)

	// Kubernetes-style health check endpoints (aliases)
	e.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "billing"})
	})
	e.GET("/readyz", s.readyHandler)
}

func (s *Server) readyHandler(c *gin.Context) {
	ctx := c.Request.Context()

	// Check database (critical)
	var one int
	if s.runtime != nil && s.runtime.DB != nil {
		if err := s.runtime.DB.GetDB().NewSelect().ColumnExpr("1").Scan(ctx, &one); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
			return
		}
	} else {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
		return
	}

	// Check Redis (critical for billing operations)
	if s.runtime != nil && s.runtime.RedisClient != nil {
		if _, err := s.runtime.RedisClient.Ping(ctx).Result(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
			return
		}
	} else {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
		return
	}

	// Check AuthKit verifier (critical for authentication)
	if s.authProvider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}
