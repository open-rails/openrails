package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
)

func verifyCaptchaChallenge(c *gin.Context, cfg *config.CaptchaConfig, verifier captcha.Verifier, challengeStore *captcha.ChallengeStore, rateStore *RateLimitStore, rdb *redis.Client, bucket, clientIP string, subjectKeys []string) bool {
	token := strings.TrimSpace(c.GetHeader(captcha.TokenHeader))
	if token == "" {
		writeCaptchaRequired(c, cfg, bucket)
		return false
	}
	if verifier == nil {
		writeCaptchaInvalid(c, "captcha verifier unavailable")
		return false
	}

	result, err := verifier.Verify(c.Request.Context(), captcha.VerifyRequest{Token: token, RemoteIP: clientIP, Bucket: bucket})
	if err != nil {
		log.WithError(err).WithField("bucket", bucket).Warn("captcha verification failed")
		writeCaptchaInvalid(c, "captcha verification failed")
		return false
	}

	if result == nil || !result.Success {
		writeCaptchaInvalid(c, "captcha invalid")
		return false
	}
	for _, subjectKey := range subjectKeys {
		if err := challengeStore.ClearChallenged(c.Request.Context(), subjectKey); err != nil {
			log.WithError(err).WithFields(log.Fields{"bucket": bucket, "subject": subjectKey}).Warn("failed to clear captcha challenge")
		}
	}
	resetBuckets := cfg.EffectiveChallengeBuckets()
	if err := resetRedisRateLimitBuckets(c.Request.Context(), rdb, subjectKeys, resetBuckets); err != nil {
		log.WithError(err).WithField("bucket", bucket).Warn("failed to reset redis rate limit after captcha")
	}
	rateStore.ResetBuckets(subjectKeys, resetBuckets)
	return true
}

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
