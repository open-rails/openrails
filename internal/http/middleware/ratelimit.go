package middleware

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/doujins-org/ginapi/response"
	"github.com/gin-gonic/gin"
	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
)

const maxInMemoryRateLimitCounters = 10_000

// RateLimitStore holds in-memory counters as a fallback when Redis is unavailable.
type RateLimitStore struct {
	mu       sync.Mutex
	counters map[string]*inMemoryCounter
}

type inMemoryCounter struct {
	count int
	reset time.Time
}

type rateLimitResult struct {
	allowed   bool
	remaining int
	reset     time.Duration
	count     int
}

// NewRateLimitStore creates a new in-memory fallback store.
func NewRateLimitStore() *RateLimitStore {
	return &RateLimitStore{counters: make(map[string]*inMemoryCounter)}
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
	return func(c *gin.Context) {
		limit, bucket := resolveRateLimitPolicy(rateLimiterConfig, c.Request)
		if limit == nil {
			c.Next()
			return
		}

		clientIP := getClientIP(c)
		captchaTriggerEnabled := captcha.ShouldApply(captchaConfig, bucket)
		captchaEnforced := captchaShouldEnforce(captchaConfig, c.Request, bucket)
		if captchaEnforced && challengeStore != nil {
			challenged, err := challengeStore.IsChallenged(c.Request.Context(), clientIP)
			if err != nil {
				log.WithError(err).WithField("bucket", bucket).Warn("captcha challenge lookup failed")
			}
			if challenged {
				if !verifyCaptchaChallenge(c, captchaConfig, verifier, challengeStore, store, rdb, bucket, clientIP) {
					return
				}
			}
		}

		var result rateLimitResult
		var err error
		if rdb != nil {
			result, err = redisAllow(c.Request.Context(), rdb, clientIP, bucket, limit)
			if err != nil {
				log.WithError(err).Warn("Rate limit redis error; falling back to in-memory limiter")
			}
		}

		if rdb == nil || err != nil {
			result = store.Allow(clientIP, bucket, limit)
		}

		limitCap := effectiveLimit(limit)
		c.Header("X-RateLimit-Limit", strconv.Itoa(limitCap))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(result.remaining))
		if result.reset > 0 {
			c.Header("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(result.reset).Unix(), 10))
		}

		if !result.allowed {
			if captchaTriggerEnabled && challengeStore != nil && result.count >= captcha.ExtremeThreshold(limit, captchaConfig) {
				if err := challengeStore.MarkChallenged(c.Request.Context(), clientIP, captchaConfig.EffectiveChallengeTTL()); err != nil {
					log.WithError(err).WithField("bucket", bucket).Warn("failed to mark captcha challenge")
				}
				writeCaptchaRequired(c, captchaConfig, bucket)
				return
			}

			retryAfter := int(math.Ceil(result.reset.Seconds()))
			if retryAfter <= 0 {
				retryAfter = 60
			}
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			log.WithFields(log.Fields{
				"client_ip": clientIP,
				"path":      c.Request.URL.Path,
				"method":    c.Request.Method,
				"bucket":    bucket,
			}).Warn("Rate limit exceeded")
			response.TooManyRequests(c, "Rate limit exceeded")
			c.Abort()
			return
		}

		c.Next()
	}
}

// Allow applies a simple fixed 60-second window per IP+bucket when Redis is unavailable.
func (s *RateLimitStore) Allow(ip, bucket string, limit *config.RateLimit) rateLimitResult {
	if limit == nil {
		return rateLimitResult{allowed: true}
	}

	threshold := effectiveLimit(limit)
	if threshold <= 0 {
		return rateLimitResult{allowed: true}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := fmt.Sprintf("%s:%s", bucket, ip)
	now := time.Now()
	counter, ok := s.counters[key]
	if !ok || now.After(counter.reset) {
		s.pruneLocked(now)
		counter = &inMemoryCounter{count: 0, reset: now.Add(time.Minute)}
		s.counters[key] = counter
	}

	counter.count++
	allowed := counter.count <= threshold
	remaining := threshold - counter.count
	if remaining < 0 {
		remaining = 0
	}
	reset := time.Until(counter.reset)
	if reset < 0 {
		reset = 0
	}

	return rateLimitResult{allowed: allowed, remaining: remaining, reset: reset, count: counter.count}
}

func (s *RateLimitStore) pruneLocked(now time.Time) {
	for key, counter := range s.counters {
		if counter == nil || now.After(counter.reset) {
			delete(s.counters, key)
		}
	}
	for len(s.counters) >= maxInMemoryRateLimitCounters {
		var oldestKey string
		var oldest time.Time
		for key, counter := range s.counters {
			if counter == nil {
				oldestKey = key
				break
			}
			if oldestKey == "" || counter.reset.Before(oldest) {
				oldestKey = key
				oldest = counter.reset
			}
		}
		if oldestKey == "" {
			return
		}
		delete(s.counters, oldestKey)
	}
}

func (s *RateLimitStore) Reset(ip, bucket string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.counters, fmt.Sprintf("%s:%s", bucket, ip))
}

func (s *RateLimitStore) ResetBuckets(ip string, buckets []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, bucket := range buckets {
		bucket = strings.ToLower(strings.TrimSpace(bucket))
		if bucket == "" {
			continue
		}
		delete(s.counters, fmt.Sprintf("%s:%s", bucket, ip))
	}
}

// redisAllow implements a per-IP, per-bucket fixed-window counter in Redis (1-minute window).
func redisAllow(ctx context.Context, rdb *redis.Client, ip, bucket string, limit *config.RateLimit) (rateLimitResult, error) {
	if limit == nil {
		return rateLimitResult{allowed: true}, nil
	}
	threshold := effectiveLimit(limit)
	if threshold <= 0 {
		return rateLimitResult{allowed: true}, nil
	}
	window := currentRateLimitWindow()
	key := rateLimitRedisKey(bucket, ip, window)
	cnt, err := rdb.Incr(ctx, key).Result()
	if err != nil {
		return rateLimitResult{}, err
	}
	if cnt == 1 {
		_ = rdb.Expire(ctx, key, time.Minute)
	}
	allowed := cnt <= int64(threshold)
	remaining := threshold - int(cnt)
	if remaining < 0 {
		remaining = 0
	}
	reset := time.Until(time.Unix((window+1)*60, 0))
	return rateLimitResult{allowed: allowed, remaining: remaining, reset: reset, count: int(cnt)}, nil
}

func resetRedisRateLimitBuckets(ctx context.Context, rdb *redis.Client, ip string, buckets []string) error {
	if rdb == nil {
		return nil
	}
	keys := make([]string, 0, len(buckets))
	window := currentRateLimitWindow()
	for _, bucket := range buckets {
		bucket = strings.ToLower(strings.TrimSpace(bucket))
		if bucket == "" {
			continue
		}
		keys = append(keys, rateLimitRedisKey(bucket, ip, window))
	}
	if len(keys) == 0 {
		return nil
	}
	return rdb.Del(ctx, keys...).Err()
}

func currentRateLimitWindow() int64 {
	return time.Now().Unix() / 60
}

func rateLimitRedisKey(bucket, ip string, window int64) string {
	return fmt.Sprintf("rl:%s:%s:%d", bucket, ip, window)
}

func resolveRateLimitPolicy(cfg *config.RateLimitsConfig, req *http.Request) (*config.RateLimit, string) {
	if cfg == nil || req == nil {
		return nil, ""
	}
	bucket := classifyBucket(strings.ToLower(req.URL.Path), req.Method)
	if bucket == "captcha" {
		return nil, bucket
	}
	var limit *config.RateLimit
	switch bucket {
	case "webhook":
		limit = (*cfg)["webhook"]
	case "subscriptions":
		limit = (*cfg)["subscribe"]
	case "checkout":
		limit = (*cfg)["checkout"]
	case "payment-methods":
		limit = (*cfg)["payment"]
	default:
		limit = (*cfg)["default"]
	}
	if limit == nil {
		limit = (*cfg)["default"]
	}
	return limit, bucket
}

func classifyBucket(path, method string) string {
	// Normalize embedded/standalone prefixes into a stable matcher.
	// Examples:
	// - /v1/webhooks/...      -> /v1/webhooks/...
	// - /billing/v1/webhooks/ -> /v1/webhooks/...
	// - /billing/v1/...      -> /v1/...
	if strings.HasPrefix(path, "/billing") {
		path = strings.TrimPrefix(path, "/billing")
		if path == "" {
			path = "/"
		}
	}

	method = strings.ToUpper(method)
	switch {
	case path == "/v1/captcha/status" || path == "/v1/captcha/client.js":
		return "captcha"
	case strings.HasPrefix(path, "/v1/webhooks"):
		return "webhook"
	case strings.HasPrefix(path, "/v1/me/payment-methods"):
		return "payment-methods"
	case strings.HasPrefix(path, "/v1/me/subscriptions") && (method == http.MethodPost || method == http.MethodPut || method == http.MethodDelete):
		return "subscriptions"
	case strings.HasPrefix(path, "/v1/checkout") && method == http.MethodPost:
		return "checkout"
	default:
		return "default"
	}
}

func captchaShouldEnforce(cfg *config.CaptchaConfig, req *http.Request, bucket string) bool {
	if cfg == nil || !cfg.Enabled || req == nil || bucket == "" || bucket == "webhook" || bucket == "captcha" {
		return false
	}
	path := strings.ToLower(req.URL.Path)
	if strings.HasPrefix(path, "/billing") {
		path = strings.TrimPrefix(path, "/billing")
		if path == "" {
			path = "/"
		}
	}
	return strings.HasPrefix(path, "/v1/")
}

func effectiveLimit(limit *config.RateLimit) int {
	if limit == nil {
		return 0
	}
	if limit.RequestsPerMinute <= 0 {
		return 60 // Default to 60 requests per minute
	}
	return limit.RequestsPerMinute
}
