package middleware

// The rate-limit and captcha engine. EvaluateRateLimit returns a decision
// without writing the response; RateLimitHTTP, the one middleware for the
// standalone and embedded surfaces, writes the internal/api envelope.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

// RateLimitScopeIP / RateLimitScopeUser are the two rate-limit subject scopes. A
// request is limited independently per IP and per authenticated user, and blocked
// when EITHER trips (combineRateLimitResults takes the strictest).
const (
	RateLimitScopeIP   = "ip"
	RateLimitScopeUser = "user"
)

// BucketMaxContentLength caps each bucket's request body; a larger declared
// Content-Length gets 413 before any counting or body read. Unlisted buckets
// (e.g. "webhook", capped per rail in the handler) are not checked here.
var BucketMaxContentLength = map[string]int64{
	"checkout":        64 << 10, // 64 KiB
	"subscriptions":   64 << 10, // 64 KiB
	"payment-methods": 64 << 10, // 64 KiB
}

// RateLimitSubject is one rate-limit/captcha subject: an IP or a user.
type RateLimitSubject struct {
	Scope string
	Value string
	Key   string
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

// RateLimitOutcome is the engine's verdict; the caller writes the response.
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

// RateLimitDecision is the engine verdict. Headers holds every response header
// to set; the caller writes the status and body. On allow, SubjectKeys is
// pinned on the context so the abuse tracker reads the same subjects.
type RateLimitDecision struct {
	Outcome        RateLimitOutcome
	Bucket         string
	Headers        map[string]string
	SubjectKeys    []string
	CaptchaMessage string
	// CaptchaSolved: the request's captcha token was verified; a token
	// verifies once, so nothing later in the request asks for it again.
	CaptchaSolved bool
}

// RateLimitDeps is the engine's collaborator set. State and ChallengeStore
// are required; Verifier may be nil (no captcha).
type RateLimitDeps struct {
	Limits         *config.RateLimitsConfig
	Captcha        *config.CaptchaConfig
	State          *abusestate.Store
	ChallengeStore *captcha.ChallengeStore
	Verifier       captcha.Verifier
}

// EvaluateRateLimit decides rate limit and captcha for a request. It may wrap
// r.Body (http.MaxBytesReader needs w) and read the captcha token header, but
// never writes the response.
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
	solved := false
	if captchaEnforced && deps.ChallengeStore != nil {
		// A merchant's card-attack mode is checked behind the route's gate
		// (CardAttackCaptcha): here nothing is known of the merchant.
		challenged := false
		for _, subject := range subjects {
			if deps.ChallengeStore.IsChallenged(r.Context(), subject.Key) {
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
			solved = true
		}
	}

	results := make([]subjectRateLimitResult, 0, len(subjects))
	for _, subject := range subjects {
		result := allow(r.Context(), deps.State, subject.Key, bucket, effectiveLimit(limit))
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
	decision := RateLimitDecision{Bucket: bucket, Headers: headers, SubjectKeys: keys, CaptchaSolved: solved}

	if !combined.result.allowed {
		if captchaEnforced && deps.ChallengeStore != nil {
			extremeThreshold := captcha.ExtremeThreshold(limit, deps.Captcha)
			markedChallenge := false
			for _, item := range results {
				if !item.result.allowed && item.result.count >= extremeThreshold {
					deps.ChallengeStore.MarkChallenged(r.Context(), item.subject.Key, config.CaptchaChallengeTTL)
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

// evaluateCaptchaVerify verifies the captcha token and, on success, clears the
// challenge and resets the affected rate-limit buckets.
func evaluateCaptchaVerify(r *http.Request, deps RateLimitDeps, bucket, clientIP string, keys []string) RateLimitDecision {
	token := strings.TrimSpace(r.Header.Get(captcha.TokenHeader))
	if token == "" {
		return RateLimitDecision{Outcome: RateLimitCaptchaRequired, Headers: map[string]string{"X-Captcha-Required": "true"}}
	}
	// Verifier is non-nil whenever captcha enforcement is on;
	// TestEnabledCaptchaAlwaysHasVerifier asserts it.
	result, err := deps.Verifier.Verify(r.Context(), captcha.VerifyRequest{Token: token, RemoteIP: clientIP, Bucket: bucket})
	if err != nil {
		log.WithError(err).WithField("bucket", bucket).Warn("captcha verification failed")
		return RateLimitDecision{Outcome: RateLimitCaptchaInvalid, Headers: map[string]string{"X-Captcha-Required": "true"}, CaptchaMessage: "captcha verification failed"}
	}
	if result == nil || !result.Success {
		return RateLimitDecision{Outcome: RateLimitCaptchaInvalid, Headers: map[string]string{"X-Captcha-Required": "true"}, CaptchaMessage: "captcha invalid"}
	}

	for _, subjectKey := range keys {
		deps.ChallengeStore.ClearChallenged(r.Context(), subjectKey)
	}
	if err := deps.State.ResetCounts(r.Context(), time.Minute, bucketKeys(keys, config.CaptchaChallengeBuckets())...); err != nil {
		log.WithError(err).WithField("bucket", bucket).Warn("failed to reset rate limits after captcha")
	}
	return RateLimitDecision{Outcome: RateLimitAllow}
}

// RateLimitHTTP is the net/http rate-limit and captcha middleware; it lets the
// embedded surface enforce limits without a fronting gateway.
//
// It runs before any route authenticates its caller, so it limits each
// client IP; the admin operation limiter keys the admitted caller after the
// gate. resolver picks the client IP behind trusted proxies, so each real
// client is limited rather than the load balancer; nil or empty uses the
// socket peer. state is the process's abuse state; nil counts in memory of
// this middleware's own.
func RateLimitHTTP(limits *config.RateLimitsConfig, captchaCfg *config.CaptchaConfig, state *abusestate.Store, challengeStore *captcha.ChallengeStore, resolver *iputil.TrustedProxies) HTTPMiddleware {
	if limits == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	if state == nil {
		state = abusestate.New(nil)
	}
	if challengeStore == nil {
		challengeStore = captcha.NewChallengeStore(state)
	}
	deps := RateLimitDeps{
		Limits:         limits,
		Captcha:        captchaCfg,
		State:          state,
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
	// Error outcomes emit the canonical internal/api envelope.
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
		if decision.CaptchaSolved {
			r = r.WithContext(context.WithValue(r.Context(), captchaSolvedKey{}, true))
		}
		next.ServeHTTP(w, r)
	}
}

type captchaSolvedKey struct{}

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

// rateLimitSubjectsHTTP derives the ip: subject from the resolver's client IP
// (nil resolver: the socket peer).
func rateLimitSubjectsHTTP(r *http.Request, resolver *iputil.TrustedProxies) []RateLimitSubject {
	if r == nil {
		return nil
	}
	subjects := make([]RateLimitSubject, 0, 2)
	resolved := resolver.ClientIP(r)
	if clientIP := strings.TrimSpace(resolved); clientIP != "" {
		subjects = append(subjects, RateLimitSubject{Scope: RateLimitScopeIP, Value: clientIP, Key: RateLimitScopeIP + ":" + clientIP})
	}
	return subjects
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

// SubjectKeysFromContext returns the subject keys the rate-limit middleware
// computed for this request, or nil, so handlers mark the same captcha
// subjects it enforces.
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

// allow counts one request in subjectKey's bucket this clock minute.
func allow(ctx context.Context, state *abusestate.Store, subjectKey, bucket string, threshold int) rateLimitResult {
	if threshold <= 0 {
		return rateLimitResult{allowed: true}
	}
	count, reset := state.Count(ctx, rateLimitKey(bucket, subjectKey), 1, time.Minute)
	return rateLimitResult{allowed: count <= int64(threshold), remaining: max(threshold-int(count), 0), reset: reset, count: int(count)}
}

func rateLimitKey(bucket, subjectKey string) string {
	return "rl:" + bucket + ":" + subjectKey
}

// bucketKeys are the subjects' rate-limit keys in buckets.
func bucketKeys(subjectKeys []string, buckets []string) []string {
	keys := make([]string, 0, len(buckets)*len(subjectKeys))
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
			keys = append(keys, rateLimitKey(bucket, subjectKey))
		}
	}
	return keys
}

func resolveRateLimitPolicy(cfg *config.RateLimitsConfig, req *http.Request) (*config.RateLimit, string) {
	if cfg == nil || req == nil {
		return nil, ""
	}
	bucket := ClassifyBucket(strings.ToLower(policyRequestPath(req)), req.Method)
	switch bucket {
	case "webhook":
		return (*cfg)["webhook"], bucket
	case "subscriptions":
		return (*cfg)["subscribe"], bucket
	case "checkout":
		return (*cfg)["checkout"], bucket
	case "payment-methods":
		return (*cfg)["payment"], bucket
	case "metrics-ask", "catalog-ask":
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
	case method == http.MethodPost && path == "/v1/metrics/ask":
		return "metrics-ask"
	case method == http.MethodPost && path == "/v1/catalog/ask":
		return "catalog-ask"
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

// isCheckoutPath is a buyer paying: creating an order, paying or confirming
// one, or a wallet's Solana Pay request for one.
func isCheckoutPath(path string) bool {
	if path == "/v1/me/orders" {
		return true
	}
	rest, mine := strings.CutPrefix(path, "/v1/me/orders/")
	if mine {
		return strings.HasSuffix(rest, "/pay") || strings.HasSuffix(rest, "/confirm")
	}
	return strings.HasPrefix(path, "/v1/orders/") && strings.HasSuffix(path, "/solana-pay")
}

// CardAttackCaptcha is the captcha a merchant under a card-testing attack
// puts in front of its card routes: every request to them solves one, and one
// solve never lifts the attack. It runs behind the route's gate, which bound
// the merchant; ahead of the gate nothing is known of it, and limits key on
// the address. A captcha the address limiter verified on this request is not
// asked for again. Nil where the runtime enforces no captcha.
func CardAttackCaptcha(rt *app.Runtime) router.Middleware {
	if rt == nil || rt.Config == nil || rt.Config.RateLimits == nil || rt.CaptchaStore == nil || rt.AbuseState == nil || !config.CaptchaEnabled(rt.Config.Captcha) {
		return nil
	}
	return cardAttackCaptcha(RateLimitDeps{
		Limits: rt.Config.RateLimits, Captcha: rt.Config.Captcha, State: rt.AbuseState, ChallengeStore: rt.CaptchaStore,
		Verifier: captcha.NewVerifier(rt.Config.Captcha, nil),
	})
}

func cardAttackCaptcha(deps RateLimitDeps) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			ctx := r.Request.Context()
			limit, bucket := resolveRateLimitPolicy(deps.Limits, r.Request)
			mid, bound := merchant.FromContext(ctx)
			solved, _ := ctx.Value(captchaSolvedKey{}).(bool)
			if !bound || limit == nil || solved || !captcha.ShouldApply(deps.Captcha, bucket) ||
				!deps.ChallengeStore.IsChallenged(ctx, captcha.CardAttackModeSubject(mid.UUID())) {
				next(r)
				return
			}
			verdict := evaluateCaptchaVerify(r.Request, deps, bucket, r.ClientIP(), SubjectKeysFromContext(ctx))
			if verdict.Outcome == RateLimitAllow {
				next(r)
				return
			}
			verdict.Bucket = bucket
			r.Through(func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					applyRateLimitDecisionHTTP(w, req, nil, verdict, deps.Captcha)
				})
			}, func(*request.Request) {})
		}
	}
}

func effectiveLimit(limit *config.RateLimit) int {
	if limit == nil {
		return 0
	}
	if limit.RequestsPerMinute <= 0 {
		return 60
	}
	return limit.RequestsPerMinute
}

// Limits per hosted checkout URL per minute: its secret is the credential, so
// it is limited whatever address presents it. Polling reads every three
// seconds; a pay is a buyer's click.
const (
	CheckoutURLReadsPerMinute = 120
	CheckoutURLPaysPerMinute  = 10
)

// CheckoutURLRateLimit limits the requests under one hosted checkout URL,
// keyed by its order (key; "" passes), whatever address presents it.
func CheckoutURLRateLimit(rt *app.Runtime, bucket string, perMinute int, key func(*request.Request) string) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			order := key(r)
			if rt == nil || rt.Config == nil || rt.Config.RateLimits == nil || order == "" || limitSubject(r, rt, "cks:"+order, bucket, perMinute) {
				next(r)
			}
		}
	}
}

// limitSubject counts one request of subject in bucket; false answered 429.
func limitSubject(r *request.Request, rt *app.Runtime, subject, bucket string, perMinute int) bool {
	result := allow(r.Request.Context(), rt.AbuseState, subject, bucket, perMinute)
	if result.allowed {
		return true
	}
	retryAfter := int(math.Ceil(result.reset.Seconds()))
	if retryAfter <= 0 {
		retryAfter = 60
	}
	r.SetHeader("Retry-After", strconv.Itoa(retryAfter))
	r.AbortCode(billing.CodeRateLimitExceeded, "Rate limit exceeded")
	return false
}
