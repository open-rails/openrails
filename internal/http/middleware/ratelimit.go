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
)

// RateLimitStore holds in-memory counters as a fallback when Redis is unavailable.
type RateLimitStore struct {
	mu       sync.Mutex
	counters map[string]*inMemoryCounter
}

type inMemoryCounter struct {
	count int
	reset time.Time
}

// NewRateLimitStore creates a new in-memory fallback store.
func NewRateLimitStore() *RateLimitStore {
	return &RateLimitStore{counters: make(map[string]*inMemoryCounter)}
}

// RateLimit enforces per-bucket limits using Redis when available and falling back to an in-memory window.
func RateLimit(rateLimiterConfig *config.RateLimitsConfig, rdb *redis.Client) gin.HandlerFunc {
	if rateLimiterConfig == nil {
		return func(c *gin.Context) { c.Next() }
	}

	store := NewRateLimitStore()

	return func(c *gin.Context) {
		limit, bucket := resolveRateLimitPolicy(rateLimiterConfig, c.Request)
		if limit == nil {
			c.Next()
			return
		}

		clientIP := getClientIP(c)
		var (
			allowed   bool
			remaining int
			reset     time.Duration
			err       error
		)

		if rdb != nil {
			allowed, remaining, reset, err = redisAllow(c.Request.Context(), rdb, clientIP, bucket, limit)
			if err != nil {
				log.WithError(err).Warn("Rate limit redis error; falling back to in-memory limiter")
			}
		}

		if rdb == nil || err != nil {
			allowed, remaining, reset = store.Allow(clientIP, bucket, limit)
		}

		limitCap := effectiveLimit(limit)
		c.Header("X-RateLimit-Limit", strconv.Itoa(limitCap))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
		if reset > 0 {
			c.Header("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(reset).Unix(), 10))
		}

		if !allowed {
			retryAfter := int(math.Ceil(reset.Seconds()))
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
func (s *RateLimitStore) Allow(ip, bucket string, limit *config.RateLimit) (bool, int, time.Duration) {
	if limit == nil {
		return true, 0, 0
	}

	threshold := effectiveLimit(limit)
	if threshold <= 0 {
		return true, 0, 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := fmt.Sprintf("%s:%s", bucket, ip)
	now := time.Now()
	counter, ok := s.counters[key]
	if !ok || now.After(counter.reset) {
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

	return allowed, remaining, reset
}

// redisAllow implements a per-IP, per-bucket fixed-window counter in Redis (1-minute window).
func redisAllow(ctx context.Context, rdb *redis.Client, ip, bucket string, limit *config.RateLimit) (bool, int, time.Duration, error) {
	if limit == nil {
		return true, 0, 0, nil
	}
	threshold := effectiveLimit(limit)
	if threshold <= 0 {
		return true, 0, 0, nil
	}
	window := time.Now().Unix() / 60 // minute window
	key := fmt.Sprintf("rl:%s:%s:%d", bucket, ip, window)
	cnt, err := rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, 0, 0, err
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
	return allowed, remaining, reset, nil
}

func resolveRateLimitPolicy(cfg *config.RateLimitsConfig, req *http.Request) (*config.RateLimit, string) {
	if cfg == nil || req == nil {
		return nil, ""
	}
	bucket := classifyBucket(strings.ToLower(req.URL.Path), req.Method)
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
	case strings.HasPrefix(path, "/v1/webhooks"):
		return "webhook"
	case strings.HasPrefix(path, "/v1/payment-methods"):
		return "payment-methods"
	case strings.HasPrefix(path, "/v1/subscriptions") && method == http.MethodPost:
		return "subscriptions"
	case (strings.HasPrefix(path, "/v1/checkout") || strings.HasPrefix(path, "/v1/me/checkout")) && method == http.MethodPost:
		return "checkout"
	default:
		return "default"
	}
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
