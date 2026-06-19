package ginmw

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	redis "github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/response"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
)

// The framework-NEUTRAL rate-limit + captcha engine lives in
// internal/http/middleware (RateLimitDecision / EvaluateRateLimit). This file is
// the thin GIN adapter over it: it derives subjects from the gin context and
// writes gin responses. The gin-free embedded surface drives the SAME engine via
// middleware.RateLimitHTTP, so the security-critical counting/captcha math is
// shared, not duplicated.

// Re-exports so existing callers/tests keep their gin-package symbols while the
// engine itself lives in the neutral package.
type RateLimitStore = middleware.RateLimitStore

var (
	// NewRateLimitStore builds the in-memory fallback counter store.
	NewRateLimitStore = middleware.NewRateLimitStore
	// SubjectKeysFromContext returns the rate-limit subject keys the middleware
	// computed for the request (#371). Handlers read it via this gin-package name.
	SubjectKeysFromContext = middleware.SubjectKeysFromContext

	// bucketMaxContentLength is the per-bucket payload ceiling (exposed for tests).
	bucketMaxContentLength = middleware.BucketMaxContentLength
)

// classifyBucket maps a path+method to a rate-limit bucket (gin-package shim over
// the neutral classifier, kept for the existing ginmw tests).
func classifyBucket(path, method string) string {
	return middleware.ClassifyBucket(path, method)
}

// RateLimit enforces per-bucket limits using Redis when available and falling back to an in-memory window.
func RateLimit(rateLimiterConfig *config.RateLimitsConfig, captchaConfig *config.CaptchaConfig, rdb *redis.Client) gin.HandlerFunc {
	return RateLimitWithChallengeStore(rateLimiterConfig, captchaConfig, rdb, captcha.NewChallengeStore(rdb))
}

func RateLimitWithChallengeStore(rateLimiterConfig *config.RateLimitsConfig, captchaConfig *config.CaptchaConfig, rdb *redis.Client, challengeStore *captcha.ChallengeStore) gin.HandlerFunc {
	if rateLimiterConfig == nil {
		return func(c *gin.Context) { c.Next() }
	}

	if challengeStore == nil {
		challengeStore = captcha.NewChallengeStore(rdb)
	}
	store := NewRateLimitStore()
	verifier := captcha.NewVerifier(captchaConfig, nil)
	return rateLimitWithDependencies(rateLimiterConfig, captchaConfig, rdb, store, challengeStore, verifier)
}

func rateLimitWithDependencies(rateLimiterConfig *config.RateLimitsConfig, captchaConfig *config.CaptchaConfig, rdb *redis.Client, store *RateLimitStore, challengeStore *captcha.ChallengeStore, verifier captcha.Verifier) gin.HandlerFunc {
	deps := middleware.RateLimitDeps{
		Limits:         rateLimiterConfig,
		Captcha:        captchaConfig,
		RDB:            rdb,
		Store:          store,
		ChallengeStore: challengeStore,
		Verifier:       verifier,
	}
	return func(c *gin.Context) {
		subjects := rateLimitSubjects(c)
		decision := middleware.EvaluateRateLimit(c.Writer, c.Request, subjects, deps)
		applyGinDecision(c, decision, captchaConfig)
	}
}

// applyGinDecision writes the gin response for an engine decision (or pins the
// subject keys + calls the next handler on allow).
func applyGinDecision(c *gin.Context, decision middleware.RateLimitDecision, captchaConfig *config.CaptchaConfig) {
	for k, v := range decision.Headers {
		c.Header(k, v)
	}
	switch decision.Outcome {
	case middleware.RateLimitTooLarge:
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request payload too large"})
	case middleware.RateLimitCaptchaRequired:
		writeCaptchaRequired(c, captchaConfig, decision.Bucket)
	case middleware.RateLimitCaptchaInvalid:
		writeCaptchaInvalid(c, decision.CaptchaMessage)
	case middleware.RateLimitTooMany:
		response.TooManyRequests(c, "Rate limit exceeded")
		c.Abort()
	default:
		if len(decision.SubjectKeys) > 0 {
			c.Request = c.Request.WithContext(middleware.WithSubjectKeys(c.Request.Context(), decision.SubjectKeys))
		}
		c.Next()
	}
}

// RateLimitSubjectKeys returns the current request's captcha/rate-limit subjects.
func RateLimitSubjectKeys(c *gin.Context) []string {
	return middleware.SubjectKeys(rateLimitSubjects(c))
}

// rateLimitSubjects derives the ip:/user: subjects from the gin context. The user
// subject is read from the gin auth keys (ginauth), the IP from the gin client IP.
func rateLimitSubjects(c *gin.Context) []middleware.RateLimitSubject {
	clientIP := strings.TrimSpace(getClientIP(c))
	subjects := make([]middleware.RateLimitSubject, 0, 2)
	if clientIP != "" {
		subjects = append(subjects, middleware.RateLimitSubject{Scope: middleware.RateLimitScopeIP, Value: clientIP, Key: middleware.RateLimitScopeIP + ":" + clientIP})
	}
	if uc, ok := ginauth.UserContextFromGin(c); ok {
		userID := strings.TrimSpace(uc.UserID)
		if userID != "" {
			subjects = append(subjects, middleware.RateLimitSubject{Scope: middleware.RateLimitScopeUser, Value: userID, Key: middleware.RateLimitScopeUser + ":" + userID})
		}
	}
	return subjects
}
