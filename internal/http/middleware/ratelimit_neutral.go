package middleware

// This file holds the rate-limit + captcha engine (issue #282). It is the
// single source of truth for OpenRails' rate-limiting and captcha enforcement;
// since #670 one net/http middleware (RateLimitHTTP) serves the standalone and
// embedded surfaces alike. The engine returns a RateLimitDecision WITHOUT
// writing the response; the middleware writes the canonical internal/api envelope.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-rails/openrails/billing"

	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

const maxInMemoryRateLimitCounters = 10_000

// RateLimitScopeIP / RateLimitScopeUser are the two rate-limit subject scopes. A
// request is limited independently per IP and per authenticated user, and blocked
// when EITHER trips (combineRateLimitResults takes the strictest).
const (
	RateLimitScopeIP   = "ip"
	RateLimitScopeUser = "user"
)

// BucketMaxContentLength is the per-bucket Content-Length ceiling used for early
// payload-size throttling. A request whose declared Content-Length exceeds the
// ceiling is rejected with 413 before any rate-limit counting or body read,
// cheaply shedding oversized-payload load. Buckets absent from the map (e.g.
// "webhook", which enforces tighter per-rail caps in the handler) are not
// checked here.
var BucketMaxContentLength = map[string]int64{
	"checkout":        64 << 10, // 64 KiB
	"subscriptions":   64 << 10, // 64 KiB
	"payment-methods": 64 << 10, // 64 KiB
}

// RateLimitSubject is one rate-limit/captcha subject (an IP or a user). Both
// surfaces build these from their own identity source and pass them to the engine.
type RateLimitSubject struct {
	Scope string
	Value string
	Key   string
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

type subjectRateLimitResult struct {
	subject RateLimitSubject
	result  rateLimitResult
}

// NewRateLimitStore creates a new in-memory fallback store.
func NewRateLimitStore() *RateLimitStore {
	return &RateLimitStore{counters: make(map[string]*inMemoryCounter)}
}

// RateLimitOutcome is the verdict the engine returns; the caller maps it to a
// framework-specific response.
type RateLimitOutcome int

const (
	// RateLimitAllow lets the request proceed (the caller pins SubjectKeys and
	// calls the next handler).
	RateLimitAllow RateLimitOutcome = iota
	// RateLimitTooLarge rejects an oversized declared payload with 413.
	RateLimitTooLarge
	// RateLimitTooMany rejects a rate-limited request with 429 + Retry-After.
	RateLimitTooMany
	// RateLimitCaptchaRequired demands a captcha solve (403, X-Captcha-Required).
	RateLimitCaptchaRequired
	// RateLimitCaptchaInvalid rejects a failed captcha solve (403).
	RateLimitCaptchaInvalid
)

// RateLimitDecision is the engine verdict. Headers holds every response header to
// set (X-RateLimit-*, Retry-After, X-Captcha-Required); the caller writes the
// status + body for the outcome. SubjectKeys is pinned on the downstream context
// on the allow path (the abuse tracker, #371, reads the SAME subjects).
type RateLimitDecision struct {
	Outcome        RateLimitOutcome
	Bucket         string
	Headers        map[string]string
	SubjectKeys    []string
	CaptchaMessage string
}

// RateLimitDeps is the engine's collaborator set. Store + ChallengeStore are
// required; RDB/Verifier may be nil (in-memory fallback / no captcha).
type RateLimitDeps struct {
	Limits         *config.RateLimitsConfig
	Captcha        *config.CaptchaConfig
	RDB            *redis.Client
	Store          *RateLimitStore
	ChallengeStore *captcha.ChallengeStore
	Verifier       captcha.Verifier
}

// EvaluateRateLimit runs the full rate-limit + captcha decision for a request. It
// may wrap r.Body (oversized-payload shedding via http.MaxBytesReader, which needs
// w) and read the captcha token header, but it NEVER writes the response — the
// caller applies the decision. subjects are framework-derived (gin keys vs request
// context) and passed in so the engine stays gin-free.
func EvaluateRateLimit(w http.ResponseWriter, r *http.Request, subjects []RateLimitSubject, deps RateLimitDeps) RateLimitDecision {
	limit, bucket := resolveRateLimitPolicy(deps.Limits, r)
	if limit == nil {
		return RateLimitDecision{Outcome: RateLimitAllow, Bucket: bucket}
	}

	// Payload-size throttling: enforce per-bucket caps for both declared and
	// streaming/chunked payloads. The body wrap happens for every bucket with a
	// cap (so chunked bodies are capped on read); a declared Content-Length over
	// the cap is rejected here, before any rate-limit counting.
	if maxBytes, ok := BucketMaxContentLength[bucket]; ok && r != nil {
		maxBytes = requestBodyLimit(r, maxBytes)
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		if r.ContentLength > maxBytes {
			log.WithFields(log.Fields{
				"bucket":         bucket,
				"content_length": r.ContentLength,
				"max":            maxBytes,
			}).Warn("Request rejected: Content-Length exceeds route threshold")
			return RateLimitDecision{
				Outcome: RateLimitTooLarge,
				Bucket:  bucket,
				Headers: map[string]string{"Retry-After": "60"},
			}
		}
	}

	keys := SubjectKeys(subjects)
	clientIP := ""
	userID := ""
	for _, subject := range subjects {
		switch subject.Scope {
		case RateLimitScopeIP:
			clientIP = subject.Value
		case RateLimitScopeUser:
			userID = subject.Value
		}
	}

	// A captcha gates only the browser buckets (checkout, payment methods,
	// subscriptions). Merchant, console and server-to-server API traffic is
	// never challenged: an API key cannot solve a captcha.
	captchaEnforced := captcha.ShouldApply(deps.Captcha, bucket)
	if captchaEnforced && deps.ChallengeStore != nil {
		challenged := false
		// Card-testing attack mode (#371): while the request's merchant is under
		// attack, every request to its captcha buckets must solve a captcha. The
		// flag lives on its own subject so an individual solve never clears it.
		if attack, err := cardAttackMode(r, deps.ChallengeStore); err != nil {
			log.WithError(err).WithField("bucket", bucket).Warn("attack-mode lookup failed")
		} else if attack {
			challenged = true
		}
		for _, subject := range subjects {
			subjectChallenged, err := deps.ChallengeStore.IsChallenged(r.Context(), subject.Key)
			if err != nil {
				log.WithError(err).WithFields(log.Fields{"bucket": bucket, "subject": subject.Key}).Warn("captcha challenge lookup failed")
			}
			if subjectChallenged {
				challenged = true
			}
		}
		if challenged {
			verdict := evaluateCaptchaVerify(r, deps, bucket, clientIP, keys)
			if verdict.Outcome != RateLimitAllow {
				verdict.Bucket = bucket
				return verdict
			}
			// Solved: fall through to normal counting (with the buckets just reset).
		}
	}

	results := make([]subjectRateLimitResult, 0, len(subjects))
	for _, subject := range subjects {
		var result rateLimitResult
		var err error
		if deps.RDB != nil {
			result, err = redisAllow(r.Context(), deps.RDB, subject.Key, bucket, limit)
			if err != nil {
				log.WithError(err).WithField("subject", subject.Key).Warn("Rate limit redis error; falling back to in-memory limiter")
			}
		}
		if deps.RDB == nil || err != nil {
			result = deps.Store.Allow(subject.Key, bucket, limit)
		}
		results = append(results, subjectRateLimitResult{subject: subject, result: result})
	}
	combined := combineRateLimitResults(results)

	headers := map[string]string{
		"X-RateLimit-Limit":     strconv.Itoa(effectiveLimit(limit)),
		"X-RateLimit-Remaining": strconv.Itoa(combined.result.remaining),
	}
	if combined.result.reset > 0 {
		headers["X-RateLimit-Reset"] = strconv.FormatInt(time.Now().Add(combined.result.reset).Unix(), 10)
	}
	decision := RateLimitDecision{Bucket: bucket, Headers: headers, SubjectKeys: keys}

	if !combined.result.allowed {
		if captchaEnforced && deps.ChallengeStore != nil {
			extremeThreshold := captcha.ExtremeThreshold(limit, deps.Captcha)
			markedChallenge := false
			for _, item := range results {
				if !item.result.allowed && item.result.count >= extremeThreshold {
					if err := deps.ChallengeStore.MarkChallenged(r.Context(), item.subject.Key, config.CaptchaChallengeTTL); err != nil {
						log.WithError(err).WithFields(log.Fields{"bucket": bucket, "subject": item.subject.Key}).Warn("failed to mark captcha challenge")
					}
					markedChallenge = true
				}
			}
			if markedChallenge {
				headers["X-Captcha-Required"] = "true"
				decision.Outcome = RateLimitCaptchaRequired
				return decision
			}
		}

		retryAfter := int(math.Ceil(combined.result.reset.Seconds()))
		if retryAfter <= 0 {
			retryAfter = 60
		}
		headers["Retry-After"] = strconv.Itoa(retryAfter)
		log.WithFields(log.Fields{
			"limited_subject": combined.subject.Key,
			"client_ip":       clientIP,
			"user_id":         userID,
			"path":            LogPath(r),
			"method":          r.Method,
			"bucket":          bucket,
		}).Warn("Rate limit exceeded")
		decision.Outcome = RateLimitTooMany
		return decision
	}

	decision.Outcome = RateLimitAllow
	return decision
}

// evaluateCaptchaVerify is the gin-free analogue of the old verifyCaptchaChallenge:
// it reads the captcha token, verifies it, and on success clears the challenge +
// resets the affected rate-limit buckets. It returns a decision (RateLimitAllow on
// success) instead of writing a response.
func evaluateCaptchaVerify(r *http.Request, deps RateLimitDeps, bucket, clientIP string, keys []string) RateLimitDecision {
	token := strings.TrimSpace(r.Header.Get(captcha.TokenHeader))
	if token == "" {
		return RateLimitDecision{Outcome: RateLimitCaptchaRequired, Headers: map[string]string{"X-Captcha-Required": "true"}}
	}
	// or#865: a `deps.Verifier == nil` leg used to sit here. It could not fire in
	// any configuration — this function is only reached when captcha enforcement
	// is on (captcha.ShouldApply ⇒ cfg.IsEnabled()), and captcha.NewVerifier
	// returns nil only when that same flag is off. A branch that cannot fail is
	// worse than none: it reads as protection and stops anyone looking. The
	// coupling it silently depended on is now asserted where it CAN fail —
	// TestEnabledCaptchaAlwaysHasVerifier in captcha_wiring_test.go.
	result, err := deps.Verifier.Verify(r.Context(), captcha.VerifyRequest{Token: token, RemoteIP: clientIP, Bucket: bucket})
	if err != nil {
		log.WithError(err).WithField("bucket", bucket).Warn("captcha verification failed")
		return RateLimitDecision{Outcome: RateLimitCaptchaInvalid, Headers: map[string]string{"X-Captcha-Required": "true"}, CaptchaMessage: "captcha verification failed"}
	}
	if result == nil || !result.Success {
		return RateLimitDecision{Outcome: RateLimitCaptchaInvalid, Headers: map[string]string{"X-Captcha-Required": "true"}, CaptchaMessage: "captcha invalid"}
	}

	for _, subjectKey := range keys {
		if err := deps.ChallengeStore.ClearChallenged(r.Context(), subjectKey); err != nil {
			log.WithError(err).WithFields(log.Fields{"bucket": bucket, "subject": subjectKey}).Warn("failed to clear captcha challenge")
		}
	}
	resetBuckets := config.CaptchaChallengeBuckets()
	if err := resetRedisRateLimitBuckets(r.Context(), deps.RDB, keys, resetBuckets); err != nil {
		log.WithError(err).WithField("bucket", bucket).Warn("failed to reset redis rate limit after captcha")
	}
	deps.Store.ResetBuckets(keys, resetBuckets)
	return RateLimitDecision{Outcome: RateLimitAllow}
}

// RateLimitHTTP is the gin-free net/http rate-limit + captcha middleware (issue
// #282; the ONLY rate-limit middleware since #670) — same buckets, same
// payload-size shedding, same X-RateLimit-* headers, same captcha challenge flow,
// same 429/Retry-After — and is what lets the EMBEDDED surface enforce OpenRails'
// own rate-limiting and captcha without the host fronting it with a gateway.
//
// Identity for the user-scoped subject is read from the request context
// (billingauth.FromContext), so mount billingauth.Optional BEFORE this so an
// authenticated caller is limited per-user, not only per-IP.
//
// resolver is the #746 proxy-aware client-IP resolver: the IP-scoped subject
// key is the resolved client, not the raw socket peer, so a deployment behind
// a configured trusted proxy still limits per real client instead of
// collapsing every request onto the load balancer's one address. A nil/empty
// resolver falls back to the socket peer (equivalent to no proxy trust).
func RateLimitHTTP(limits *config.RateLimitsConfig, captchaCfg *config.CaptchaConfig, rdb *redis.Client, challengeStore *captcha.ChallengeStore, resolver *iputil.TrustedProxies) HTTPMiddleware {
	if limits == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	if challengeStore == nil {
		challengeStore = captcha.NewChallengeStore(rdb)
	}
	deps := RateLimitDeps{
		Limits:         limits,
		Captcha:        captchaCfg,
		RDB:            rdb,
		Store:          NewRateLimitStore(),
		ChallengeStore: challengeStore,
		Verifier:       captcha.NewVerifier(captchaCfg, nil),
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			subjects := rateLimitSubjectsHTTP(r, resolver)
			decision := EvaluateRateLimit(w, r, subjects, deps)
			applyRateLimitDecisionHTTP(w, r, next, decision, captchaCfg)
		})
	}
}

// applyRateLimitDecisionHTTP writes the net/http response for a decision (or calls
// next on allow, pinning the subject keys for downstream abuse tracking).
func applyRateLimitDecisionHTTP(w http.ResponseWriter, r *http.Request, next http.Handler, decision RateLimitDecision, captchaCfg *config.CaptchaConfig) {
	for k, v := range decision.Headers {
		w.Header().Set(k, v)
	}
	// Error outcomes emit the canonical internal/api envelope — identical to the
	// retired gin middleware's writers, so the standalone flip (#670) changed no
	// response bodies (and the embedded surface now matches too).
	switch decision.Outcome {
	case RateLimitTooLarge:
		apiErr := api.Coded(billing.CodeRequestBodyTooLarge, "request payload too large")
		writeJSONResponse(w, apiErr.HTTPStatus, apiErr.ToResponse())
	case RateLimitCaptchaRequired:
		w.Header().Set("X-Captcha-Required", "true")
		apiErr := api.NewAPIError(http.StatusForbidden, api.ErrorTypeInvalidRequest, "captcha_required", "Captcha verification required").
			WithMetadata(map[string]any{
				"provider": config.CaptchaProvider(captchaCfg),
				"site_key": strings.TrimSpace(captchaSiteKey(captchaCfg)),
				"bucket":   decision.Bucket,
			})
		writeJSONResponse(w, apiErr.HTTPStatus, apiErr.ToResponse())
	case RateLimitCaptchaInvalid:
		w.Header().Set("X-Captcha-Required", "true")
		msg := strings.TrimSpace(decision.CaptchaMessage)
		if msg == "" {
			msg = "Captcha verification failed"
		}
		apiErr := api.NewAPIError(http.StatusForbidden, api.ErrorTypeInvalidRequest, "captcha_invalid", msg)
		writeJSONResponse(w, apiErr.HTTPStatus, apiErr.ToResponse())
	case RateLimitTooMany:
		apiErr := api.Coded(billing.CodeRateLimitExceeded, "Rate limit exceeded")
		writeJSONResponse(w, apiErr.HTTPStatus, apiErr.ToResponse())
	default:
		if len(decision.SubjectKeys) > 0 {
			r = r.WithContext(WithSubjectKeys(r.Context(), decision.SubjectKeys))
		}
		next.ServeHTTP(w, r)
	}
}

func writeJSONResponse(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func captchaSiteKey(cfg *config.CaptchaConfig) string {
	if cfg == nil {
		return ""
	}
	return cfg.SiteKey
}

// rateLimitSubjectsHTTP derives the ip:/user: subjects from a plain request,
// reading identity from the request context (billingauth) and the client IP
// via resolver (#746: a nil/empty resolver trusts nothing, i.e. the socket peer).
func rateLimitSubjectsHTTP(r *http.Request, resolver *iputil.TrustedProxies) []RateLimitSubject {
	if r == nil {
		return nil
	}
	subjects := make([]RateLimitSubject, 0, 2)
	resolved := resolver.ClientIP(r)
	if clientIP := strings.TrimSpace(resolved); clientIP != "" {
		subjects = append(subjects, RateLimitSubject{Scope: RateLimitScopeIP, Value: clientIP, Key: RateLimitScopeIP + ":" + clientIP})
	}
	if uc, ok := billingauth.FromContext(r.Context()); ok {
		if userID := strings.TrimSpace(uc.UserID); userID != "" {
			subjects = append(subjects, RateLimitSubject{Scope: RateLimitScopeUser, Value: userID, Key: RateLimitScopeUser + ":" + userID})
		}
	}
	return subjects
}

// RateLimitSubjectKeysHTTP is the gin-free analogue of RateLimitSubjectKeys (issue
// #282): it derives the same ip:/user: subject keys from a plain *http.Request. The
// embedded captcha-status handler uses it to report whether a subject is currently
// challenged; resolver MUST be the same one RateLimitHTTP was built with, or the
// keys diverge.
func RateLimitSubjectKeysHTTP(r *http.Request, resolver *iputil.TrustedProxies) []string {
	return SubjectKeys(rateLimitSubjectsHTTP(r, resolver))
}

// SubjectKeys returns the non-empty Key of each subject.
func SubjectKeys(subjects []RateLimitSubject) []string {
	keys := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		if subject.Key != "" {
			keys = append(keys, subject.Key)
		}
	}
	return keys
}

type subjectKeysCtxKey struct{}

// WithSubjectKeys pins the rate-limit subject keys for downstream handlers.
func WithSubjectKeys(ctx context.Context, keys []string) context.Context {
	return context.WithValue(ctx, subjectKeysCtxKey{}, keys)
}

// SubjectKeysFromContext returns the rate-limit subject keys (ip:.. / user:..) the
// rate-limit middleware computed for this request, or nil. Handlers use it to
// mark/inspect the SAME captcha subjects the middleware enforces (#371).
func SubjectKeysFromContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	keys, _ := ctx.Value(subjectKeysCtxKey{}).([]string)
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
	return s.allowWindow(subjectKey, bucket, effectiveLimit(limit), time.Minute)
}

func (s *RateLimitStore) allowWindow(subjectKey, bucket string, threshold int, window time.Duration) rateLimitResult {
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
		counter = &inMemoryCounter{count: 0, reset: now.Add(window)}
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

// ResetBuckets clears the given buckets for the given subjects.
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

// Snapshot returns a copy of the live in-memory counter counts keyed by
// "bucket:subject". For tests and lightweight introspection; it never exposes the
// internal counter pointers.
func (s *RateLimitStore) Snapshot() map[string]int {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.counters))
	for key, counter := range s.counters {
		if counter != nil {
			out[key] = counter.count
		}
	}
	return out
}

// SeedCounter sets a counter's count + reset for one bucket+subject. Used by tests
// and warm-start scenarios to prime the in-memory window.
func (s *RateLimitStore) SeedCounter(bucket, subjectKey string, count int, reset time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters[rateLimitMemoryKey(bucket, subjectKey)] = &inMemoryCounter{count: count, reset: reset}
}

// redisAllow implements a per-subject, per-bucket fixed-window counter in Redis (1-minute window).
func redisAllow(ctx context.Context, rdb *redis.Client, subjectKey, bucket string, limit *config.RateLimit) (rateLimitResult, error) {
	if limit == nil {
		return rateLimitResult{allowed: true}, nil
	}
	return redisWindowAllow(ctx, rdb, subjectKey, bucket, effectiveLimit(limit), time.Minute)
}

func redisWindowAllow(ctx context.Context, rdb *redis.Client, subjectKey, bucket string, threshold int, window time.Duration) (rateLimitResult, error) {
	if threshold <= 0 {
		return rateLimitResult{allowed: true}, nil
	}
	seconds := int64(window / time.Second)
	index := time.Now().Unix() / seconds
	key := rateLimitRedisKey(bucket, subjectKey, index)
	cnt, err := rdb.Incr(ctx, key).Result()
	if err != nil {
		return rateLimitResult{}, err
	}
	if cnt == 1 {
		_ = rdb.Expire(ctx, key, window)
	}
	allowed := cnt <= int64(threshold)
	remaining := threshold - int(cnt)
	if remaining < 0 {
		remaining = 0
	}
	reset := time.Until(time.Unix((index+1)*seconds, 0))
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
	bucket := ClassifyBucket(strings.ToLower(policyRequestPath(req)), req.Method)
	if bucket == "captcha" {
		return nil, bucket
	}
	switch bucket {
	case "webhook":
		return (*cfg)["webhook"], bucket
	case "subscriptions":
		return (*cfg)["subscribe"], bucket
	case "checkout":
		return (*cfg)["checkout"], bucket
	case "payment-methods":
		return (*cfg)["payment"], bucket
	case "metrics-ask", "catalog-ask", "dashboard-generate":
		return (*cfg)[bucket], bucket
	}
	// Generic per-address ceilings belong to the proxy in front of OpenRails.
	return nil, bucket
}

// ClassifyBucket maps a request path+method to a rate-limit bucket; "" for
// a route OpenRails does not limit. It normalizes
// the embedded (/billing/v1/...) and standalone (/v1/...) prefixes to one matcher.
func ClassifyBucket(path, method string) string {
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
	case method == http.MethodPost && path == "/v1/merchant/metrics/ask":
		return "metrics-ask"
	case method == http.MethodPost && path == "/v1/merchant/catalog/ask":
		return "catalog-ask"
	case method == http.MethodPost && path == "/v1/merchant/dashboard/widgets/generate":
		return "dashboard-generate"
	case strings.HasPrefix(path, "/v1/webhooks"):
		return "webhook"
	case strings.HasPrefix(path, "/v1/me/payment-methods"):
		return "payment-methods"
	case strings.HasPrefix(path, "/v1/me/subscriptions") && (method == http.MethodPost || method == http.MethodPut || method == http.MethodDelete):
		return "subscriptions"
	case method == http.MethodPost && isCheckoutPath(path):
		return "checkout"
	default:
		return ""
	}
}

// isCheckoutPath is a buyer paying: minting a checkout session, paying one,
// or a wallet's Solana Pay request.
func isCheckoutPath(path string) bool {
	return path == "/v1/me/checkout-sessions" || strings.HasPrefix(path, "/v1/checkout-sessions/") || strings.HasPrefix(path, "/v1/checkout-attempts/")
}

// cardAttackMode reports whether the request's merchant is under a card-testing
// attack (#371). A merchant not yet known here (a delegated token names it
// later) leaves the request to its subjects' challenges and the durable
// failure ledger.
func cardAttackMode(r *http.Request, store *captcha.ChallengeStore) (bool, error) {
	id, ok := merchant.FromContext(r.Context())
	if !ok || id.IsZero() {
		return false, nil
	}
	return store.IsChallenged(r.Context(), captcha.CardAttackModeSubject(id.UUID()))
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

// Checkout session limits (#1124), per session id per minute: the id is
// the credential, so it is limited whatever address presents it. Polling reads
// every three seconds; a pay is a buyer's click.
const (
	CheckoutSessionReadsPerMinute = 120
	CheckoutSessionPaysPerMinute  = 10
)

// CheckoutSessionRateLimit limits requests naming one checkout session
// (the :id path parameter), beside the per-address limits of RateLimitHTTP.
func CheckoutSessionRateLimit(rt *app.Runtime, bucket string, perMinute int) router.Middleware {
	store := NewRateLimitStore()
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			id := r.Param("id")
			if rt == nil || rt.Config == nil || rt.Config.RateLimits == nil || !strings.HasPrefix(id, "ocs_") || len(id) > 128 {
				next(r)
				return
			}
			sum := sha256.Sum256([]byte(id))
			subject := "ocs:" + hex.EncodeToString(sum[:16])
			var result rateLimitResult
			var err error
			if rt.RedisClient != nil {
				result, err = redisWindowAllow(r.Request.Context(), rt.RedisClient, subject, bucket, perMinute, time.Minute)
			}
			if rt.RedisClient == nil || err != nil {
				result = store.allowWindow(subject, bucket, perMinute, time.Minute)
			}
			if !result.allowed {
				retryAfter := int(math.Ceil(result.reset.Seconds()))
				if retryAfter <= 0 {
					retryAfter = 60
				}
				r.SetHeader("Retry-After", strconv.Itoa(retryAfter))
				r.AbortCode(billing.CodeRateLimitExceeded, "Rate limit exceeded")
				return
			}
			next(r)
		}
	}
}

// LogPath is r's path for logs. A checkout session id is a bearer
// credential and is never logged.
func LogPath(r *http.Request) string {
	path := r.URL.Path
	i := strings.Index(path, "/ocs_")
	if i < 0 {
		return path
	}
	rest := path[i+1:]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return path[:i] + "/ocs_redacted" + rest[j:]
	}
	return path[:i] + "/ocs_redacted"
}
