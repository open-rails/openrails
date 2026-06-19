package ginmw

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/api"
)

// writeCaptchaRequired / writeCaptchaInvalid are the gin response writers for the
// captcha outcomes the neutral engine (middleware.EvaluateRateLimit) returns. The
// challenge-verification logic itself lives in the engine; this file only frames
// the gin responses. Both emit the canonical pkg/api error envelope so clients
// decode the same shape as everywhere else: the machine code (captcha_required /
// captcha_invalid) is in error.code, and the challenge parameters the client needs
// to render the widget ride in error.metadata. The X-Captcha-Required header is
// kept as a cheap detection signal that doesn't require parsing the body.

func writeCaptchaRequired(c *gin.Context, cfg *config.CaptchaConfig, bucket string) {
	c.Header("X-Captcha-Required", "true")
	apiErr := api.NewAPIError(http.StatusForbidden, api.ErrorTypeInvalidRequest, "captcha_required", "Captcha verification required").
		WithMetadata(map[string]any{
			"provider": cfg.EffectiveProvider(),
			"site_key": strings.TrimSpace(cfg.SiteKey),
			"bucket":   bucket,
		})
	c.JSON(apiErr.HTTPStatus, apiErr.ToResponse())
	c.Abort()
}

func writeCaptchaInvalid(c *gin.Context, message string) {
	c.Header("X-Captcha-Required", "true")
	if strings.TrimSpace(message) == "" {
		message = "Captcha verification failed"
	}
	apiErr := api.NewAPIError(http.StatusForbidden, api.ErrorTypeInvalidRequest, "captcha_invalid", message)
	c.JSON(apiErr.HTTPStatus, apiErr.ToResponse())
	c.Abort()
}
