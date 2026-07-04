package middleware

// This file holds the rate-limit + captcha engine (issue #282). It is the
// single source of truth for OpenRails' rate-limiting and captcha enforcement;
// since #670 one net/http middleware (RateLimitHTTP) serves the standalone and
// embedded surfaces alike. The engine returns a RateLimitDecision WITHOUT
// writing the response; the middleware writes the canonical pkg/api envelope.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/billingauth"
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
	"default":         1 << 20,  // 1 MiB (matches the global body limit)
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

	captchaTriggerEnabled := captcha.ShouldApply(deps.Captcha, bucket)
	captchaEnforced := captchaShouldEnforce(deps.Captcha, r, bucket)
	if captchaEnforced && deps.ChallengeStore != nil {
		challenged := false
		// Site-wide card-testing attack mode (#371): while active, every request to
		// a captcha-relevant bucket must solve a captcha. The flag lives on its own
		// subject so an individual solve never clears it.
		if attack, err := deps.ChallengeStore.IsChallenged(r.Context(), captcha.CardAttackModeSubject); err != nil {
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
		if captchaTriggerEnabled && deps.ChallengeStore != nil {
			extremeThreshold := captcha.ExtremeThreshold(limit, deps.Captcha)
			markedChallenge := false
			for _, item := range results {
				if !item.result.allowed && item.result.count >= extremeThreshold {
					if err := deps.ChallengeStore.MarkChallenged(r.Context(), item.subject.Key, deps.Captcha.EffectiveChallengeTTL()); err != nil {
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
			"path":            r.URL.Path,
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
	if deps.Verifier == nil {
		return RateLimitDecision{Outcome: RateLimitCaptchaInvalid, Headers: map[string]string{"X-Captcha-Required": "true"}, CaptchaMessage: "captcha verifier unavailable"}
	}

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
	resetBuckets := deps.Captcha.EffectiveChallengeBuckets()
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
	// Error outcomes emit the canonical pkg/api envelope — identical to the
	// retired gin middleware's writers, so the standalone flip (#670) changed no
	// response bodies (and the embedded surface now matches too).
	switch decision.Outcome {
	case RateLimitTooLarge:
		writeJSONResponse(w, http.StatusRequestEntityTooLarge, api.SimpleErrorResponse(http.StatusRequestEntityTooLarge, "request payload too large"))
	case RateLimitCaptchaRequired:
		w.Header().Set("X-Captcha-Required", "true")
		apiErr := api.NewAPIError(http.StatusForbidden, api.ErrorTypeInvalidRequest, "captcha_required", "Captcha verification required").
			WithMetadata(map[string]any{
				"provider": captchaCfg.EffectiveProvider(),
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
		writeJSONResponse(w, http.StatusTooManyRequests, api.SimpleErrorResponse(http.StatusTooManyRequests, "Rate limit exceeded"))
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
	resolved := resolver.ResolveClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
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
	bucket := ClassifyBucket(strings.ToLower(req.URL.Path), req.Method)
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

// ClassifyBucket maps a request path+method to a rate-limit bucket. It normalizes
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
