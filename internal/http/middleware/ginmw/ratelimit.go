package ginmw

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/internal/http/response"
	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
)

const maxInMemoryRateLimitCounters = 10_000

const (
	rateLimitScopeIP   = "ip"
	rateLimitScopeUser = "user"
)

// bucketMaxContentLength is the per-bucket Content-Length ceiling used for
// early payload-size throttling. A request whose declared Content-Length exceeds
// the ceiling is rejected with 413 before any rate-limit counting or body read,
// cheaply shedding oversized-payload load. Buckets absent from the map (e.g.
// "webhook", which enforces tighter per-processor caps in the handler) are not
// checked here.
var bucketMaxContentLength = map[string]int64{
	"checkout":        64 << 10, // 64 KiB
	"subscriptions":   64 << 10, // 64 KiB
	"payment-methods": 64 << 10, // 64 KiB
	"default":         1 << 20,  // 1 MiB (matches the global body limit)
}

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

type rateLimitSubject struct {
	Scope string
	Value string
	Key   string
}

type subjectRateLimitResult struct {
	subject rateLimitSubject
	result  rateLimitResult
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

		// Payload-size throttling: enforce per-bucket caps for both declared and
		// streaming/chunked payloads.
		if max, ok := bucketMaxContentLength[bucket]; ok && c.Request != nil {
			if c.Request.Body != nil {
				c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max)
			}
			if c.Request.ContentLength > max {
				log.WithFields(log.Fields{
					"bucket":         bucket,
					"content_length": c.Request.ContentLength,
					"max":            max,
				}).Warn("Request rejected: Content-Length exceeds route threshold")
				c.Header("Retry-After", "60")
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request payload too large"})
				return
			}
		}

		subjects := rateLimitSubjects(c)
		// Expose the exact subject keys (ip:.. / user:..) to downstream handlers
		// via the request context so abuse tracking (#371) marks the SAME
		// subjects the captcha enforcement here reads back.
		c.Request = c.Request.WithContext(
			withSubjectKeys(c.Request.Context(), subjectKeys(subjects)),
		)
		clientIP := ""
		userID := ""
		for _, subject := range subjects {
			switch subject.Scope {
			case rateLimitScopeIP:
				clientIP = subject.Value
			case rateLimitScopeUser:
				userID = subject.Value
			}
		}
		captchaTriggerEnabled := captcha.ShouldApply(captchaConfig, bucket)
		captchaEnforced := captchaShouldEnforce(captchaConfig, c.Request, bucket)
		if captchaEnforced && challengeStore != nil {
			challenged := false
			// Site-wide card-testing attack mode (#371): while active, every
			// request to a captcha-relevant bucket must solve a captcha. The flag
			// lives on its own subject so an individual solve never clears it.
			if attack, err := challengeStore.IsChallenged(c.Request.Context(), captcha.CardAttackModeSubject); err != nil {
				log.WithError(err).WithField("bucket", bucket).Warn("attack-mode lookup failed")
			} else if attack {
				challenged = true
			}
			for _, subject := range subjects {
				subjectChallenged, err := challengeStore.IsChallenged(c.Request.Context(), subject.Key)
				if err != nil {
					log.WithError(err).WithFields(log.Fields{"bucket": bucket, "subject": subject.Key}).Warn("captcha challenge lookup failed")
				}
				if subjectChallenged {
					challenged = true
				}
			}
			if challenged {
				if !verifyCaptchaChallenge(c, captchaConfig, verifier, challengeStore, store, rdb, bucket, clientIP, subjectKeys(subjects)) {
					return
				}
			}
		}

		results := make([]subjectRateLimitResult, 0, len(subjects))
		for _, subject := range subjects {
			var result rateLimitResult
			var err error
			if rdb != nil {
				result, err = redisAllow(c.Request.Context(), rdb, subject.Key, bucket, limit)
				if err != nil {
					log.WithError(err).WithField("subject", subject.Key).Warn("Rate limit redis error; falling back to in-memory limiter")
				}
			}

			if rdb == nil || err != nil {
				result = store.Allow(subject.Key, bucket, limit)
			}
			results = append(results, subjectRateLimitResult{subject: subject, result: result})
		}
		combined := combineRateLimitResults(results)

		limitCap := effectiveLimit(limit)
		c.Header("X-RateLimit-Limit", strconv.Itoa(limitCap))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(combined.result.remaining))
		if combined.result.reset > 0 {
			c.Header("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(combined.result.reset).Unix(), 10))
		}

		if !combined.result.allowed {
			if captchaTriggerEnabled && challengeStore != nil {
				extremeThreshold := captcha.ExtremeThreshold(limit, captchaConfig)
				markedChallenge := false
				for _, item := range results {
					if !item.result.allowed && item.result.count >= extremeThreshold {
						if err := challengeStore.MarkChallenged(c.Request.Context(), item.subject.Key, captchaConfig.EffectiveChallengeTTL()); err != nil {
							log.WithError(err).WithFields(log.Fields{"bucket": bucket, "subject": item.subject.Key}).Warn("failed to mark captcha challenge")
						}
						markedChallenge = true
					}
				}
				if markedChallenge {
					writeCaptchaRequired(c, captchaConfig, bucket)
					return
				}
			}

			retryAfter := int(math.Ceil(combined.result.reset.Seconds()))
			if retryAfter <= 0 {
				retryAfter = 60
			}
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			log.WithFields(log.Fields{
				"limited_subject": combined.subject.Key,
				"client_ip":       clientIP,
				"user_id":         userID,
				"path":            c.Request.URL.Path,
				"method":          c.Request.Method,
				"bucket":          bucket,
			}).Warn("Rate limit exceeded")
			response.TooManyRequests(c, "Rate limit exceeded")
			c.Abort()
			return
		}

		c.Next()
	}
}

// RateLimitSubjectKeys returns the current request's captcha/rate-limit subjects.
func RateLimitSubjectKeys(c *gin.Context) []string {
	return subjectKeys(rateLimitSubjects(c))
}

type subjectKeysCtxKey struct{}

func withSubjectKeys(ctx context.Context, keys []string) context.Context {
	return context.WithValue(ctx, subjectKeysCtxKey{}, keys)
}

// SubjectKeysFromContext returns the rate-limit subject keys (ip:.. / user:..)
// the rate-limit middleware computed for this request, or nil. Gin-free
// handlers use it to mark/inspect the SAME captcha subjects the middleware
// enforces (#371).
func SubjectKeysFromContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	keys, _ := ctx.Value(subjectKeysCtxKey{}).([]string)
	return keys
}

func rateLimitSubjects(c *gin.Context) []rateLimitSubject {
	clientIP := strings.TrimSpace(getClientIP(c))
	subjects := make([]rateLimitSubject, 0, 2)
	if clientIP != "" {
		subjects = append(subjects, rateLimitSubject{Scope: rateLimitScopeIP, Value: clientIP, Key: rateLimitScopeIP + ":" + clientIP})
	}
	if uc, ok := ginauth.UserContextFromGin(c); ok {
		userID := strings.TrimSpace(uc.UserID)
		if userID != "" {
			subjects = append(subjects, rateLimitSubject{Scope: rateLimitScopeUser, Value: userID, Key: rateLimitScopeUser + ":" + userID})
		}
	}
	return subjects
}

func subjectKeys(subjects []rateLimitSubject) []string {
	keys := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		if subject.Key != "" {
			keys = append(keys, subject.Key)
		}
	}
	return keys
}

func combineRateLimitResults(results []subjectRateLimitResult) subjectRateLimitResult {
	if len(results) == 0 {
		return subjectRateLimitResult{result: rateLimitResult{allowed: true}}
	}
	combined := results[0]
	combined.result.allowed = true
	blockedCount := -1
	for _, item := range results {
		if item.result.remaining < combined.result.remaining {
			combined.result.remaining = item.result.remaining
		}
		if item.result.reset > combined.result.reset {
			combined.result.reset = item.result.reset
		}
		if item.result.count > combined.result.count {
			combined.result.count = item.result.count
		}
		if !item.result.allowed {
			combined.result.allowed = false
			if item.result.count >= blockedCount {
				blockedCount = item.result.count
				combined.subject = item.subject
			}
		}
	}
	return combined
}

// Allow applies a simple fixed 60-second window per subject+bucket when Redis is unavailable.
func (s *RateLimitStore) Allow(subjectKey, bucket string, limit *config.RateLimit) rateLimitResult {
	if limit == nil {
		return rateLimitResult{allowed: true}
	}

	threshold := effectiveLimit(limit)
	if threshold <= 0 {
		return rateLimitResult{allowed: true}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := rateLimitMemoryKey(bucket, subjectKey)
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

func (s *RateLimitStore) Reset(subjectKey, bucket string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.counters, rateLimitMemoryKey(bucket, subjectKey))
}

func (s *RateLimitStore) ResetBuckets(subjectKeys []string, buckets []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, subjectKey := range subjectKeys {
		subjectKey = strings.TrimSpace(subjectKey)
		if subjectKey == "" {
			continue
		}
		for _, bucket := range buckets {
			bucket = strings.ToLower(strings.TrimSpace(bucket))
			if bucket == "" {
				continue
			}
			delete(s.counters, rateLimitMemoryKey(bucket, subjectKey))
		}
	}
}

// redisAllow implements a per-subject, per-bucket fixed-window counter in Redis (1-minute window).
func redisAllow(ctx context.Context, rdb *redis.Client, subjectKey, bucket string, limit *config.RateLimit) (rateLimitResult, error) {
	if limit == nil {
		return rateLimitResult{allowed: true}, nil
	}
	threshold := effectiveLimit(limit)
	if threshold <= 0 {
		return rateLimitResult{allowed: true}, nil
	}
	window := currentRateLimitWindow()
	key := rateLimitRedisKey(bucket, subjectKey, window)
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

func resetRedisRateLimitBuckets(ctx context.Context, rdb *redis.Client, subjectKeys []string, buckets []string) error {
	if rdb == nil {
		return nil
	}
	keys := make([]string, 0, len(buckets)*len(subjectKeys))
	window := currentRateLimitWindow()
	for _, subjectKey := range subjectKeys {
		subjectKey = strings.TrimSpace(subjectKey)
		if subjectKey == "" {
			continue
		}
		for _, bucket := range buckets {
			bucket = strings.ToLower(strings.TrimSpace(bucket))
			if bucket == "" {
				continue
			}
			keys = append(keys, rateLimitRedisKey(bucket, subjectKey, window))
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return rdb.Del(ctx, keys...).Err()
}

func currentRateLimitWindow() int64 {
	return time.Now().Unix() / 60
}

func rateLimitRedisKey(bucket, subjectKey string, window int64) string {
	return fmt.Sprintf("rl:%s:%s:%d", bucket, subjectKey, window)
}

func rateLimitMemoryKey(bucket, subjectKey string) string {
	return fmt.Sprintf("%s:%s", bucket, subjectKey)
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
	case strings.HasPrefix(path, "/v1/webhooks") ||
		(strings.HasPrefix(path, "/v1/merchants/") && strings.Contains(path, "/webhooks/")):
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
	if !cfg.IsEnabled() || req == nil || bucket == "" || bucket == "webhook" || bucket == "captcha" {
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
