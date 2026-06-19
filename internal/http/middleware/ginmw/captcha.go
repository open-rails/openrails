package ginmw

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/config"
)

// writeCaptchaRequired / writeCaptchaInvalid are the gin response writers for the
// captcha outcomes the neutral engine (middleware.EvaluateRateLimit) returns. The
// challenge-verification logic itself lives in the engine; this file only frames
// the gin responses.

func writeCaptchaRequired(c *gin.Context, cfg *config.CaptchaConfig, bucket string) {
	c.Header("X-Captcha-Required", "true")
	c.JSON(http.StatusForbidden, gin.H{
		"error":    "captcha_required",
		"provider": cfg.EffectiveProvider(),
		"site_key": strings.TrimSpace(cfg.SiteKey),
		"bucket":   bucket,
	})
	c.Abort()
}

func writeCaptchaInvalid(c *gin.Context, message string) {
	c.Header("X-Captcha-Required", "true")
	c.JSON(http.StatusForbidden, gin.H{
		"error":   "captcha_invalid",
		"message": message,
	})
	c.Abort()
}
