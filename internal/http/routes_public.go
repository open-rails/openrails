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
	"github.com/open-rails/openrails/internal/services/health"
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
		"enabled":           cfg.IsEnabled(),
		"required":          false,
		"token_header":      captcha.TokenHeader,
		"client_script_url": captchaClientScriptURL(c),
	}
	if cfg != nil {
		resp["provider"] = cfg.EffectiveProvider()
	}
	if !cfg.IsEnabled() || s.captchaStore == nil {
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
	enabled := cfg.IsEnabled()
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

// registerWebhookRoutesAt mounts the legacy configured-merchant webhook surface.
// It is intentionally not called by default; private standalone uses the
// merchant-scoped surface, and hosted products mount host-resolved webhooks.
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
	services, servicesReady := s.requiredServiceHealth()
	authReady := s.authProvider != nil
	verbose := c.Query("verbose") == "1" || strings.EqualFold(c.Query("verbose"), "true")
	if !servicesReady || !authReady {
		resp := gin.H{
			"status":  "not_ready",
			"service": "billing",
			"auth":    gin.H{"available": authReady},
		}
		if verbose {
			resp["dependencies"] = services
		}
		c.JSON(http.StatusServiceUnavailable, resp)
		return
	}

	resp := gin.H{
		"status":  "ready",
		"service": "billing",
		"auth":    gin.H{"available": true},
	}
	if verbose {
		resp["dependencies"] = services
	}
	c.JSON(http.StatusOK, resp)
}

func (s *Server) requiredServiceHealth() (gin.H, bool) {
	required := []string{"postgres", "garnet"}
	services := make(gin.H, len(required))
	if s == nil || s.runtime == nil || s.runtime.HealthManager == nil {
		for _, name := range required {
			services[name] = gin.H{"available": false, "reason": "health_manager_unavailable"}
		}
		return services, false
	}

	allReady := true
	for _, name := range required {
		status, exists := s.runtime.HealthManager.GetServiceHealth(name)
		if !exists {
			services[name] = gin.H{"available": false, "reason": "checker_not_registered"}
			allReady = false
			continue
		}
		services[name] = serviceHealthResponse(status)
		if !status.Available || status.CircuitOpen {
			allReady = false
		}
	}
	return services, allReady
}

func serviceHealthResponse(status *health.ServiceHealth) gin.H {
	if status == nil {
		return gin.H{"available": false}
	}
	resp := gin.H{
		"available":            status.Available && !status.CircuitOpen,
		"last_check":           status.LastCheck,
		"last_success":         status.LastSuccess,
		"consecutive_failures": status.ConsecutiveFailures,
		"circuit_open":         status.CircuitOpen,
		"next_retry_at":        status.NextRetryAt,
		"total_checks":         status.TotalChecks,
		"total_failures":       status.TotalFailures,
	}
	if status.LastError != nil {
		resp["last_error"] = status.LastError.Error()
	}
	return resp
}
