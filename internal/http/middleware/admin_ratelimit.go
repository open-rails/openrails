package middleware

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

// AdminOperation identifies one security-sensitive merchant-console action.
// Operations with the same policy still use separate counters unless they are
// deliberately assigned the same identifier (the destructive actions below
// share one aggregate blast-radius budget).
type AdminOperation string

const (
	AdminOperationDestructive AdminOperation = "destructive"
	AdminOperationExtend      AdminOperation = "extend"
	AdminOperationOffChannel  AdminOperation = "off_channel"
	AdminOperationGrant       AdminOperation = "grant"
)

const adminLockoutDuration = time.Hour

type adminRateLimitWindow struct {
	name     string
	duration time.Duration
	limit    int64
}

var adminRateLimitPolicies = map[AdminOperation][]adminRateLimitWindow{
	AdminOperationDestructive: {
		{name: "minute", duration: time.Minute, limit: 5},
		{name: "hour", duration: time.Hour, limit: 10},
		{name: "day", duration: 24 * time.Hour, limit: 50},
	},
	AdminOperationExtend: {
		{name: "minute", duration: time.Minute, limit: 3},
	},
	AdminOperationOffChannel: {
		{name: "minute", duration: time.Minute, limit: 10},
	},
	AdminOperationGrant: {
		{name: "minute", duration: time.Minute, limit: 10},
	},
}

// AdminRateLimitEvent is the structured audit/alert record emitted for every
// protected request and explicit unlock. Threshold and lockout kinds are logged
// at warning/error severity so production log alerting can page on them.
type AdminRateLimitEvent struct {
	Kind       string
	UserID     string
	ActorID    string
	MerchantID string
	Operation  AdminOperation
	Counts     map[string]int64
	RetryAfter time.Duration
	RequestID  string
}

// AdminRateLimitEventSink receives audit/alert records synchronously and must
// return quickly; external notification delivery belongs behind an async sink.
type AdminRateLimitEventSink func(context.Context, AdminRateLimitEvent)

type adminRateLimitDecision struct {
	allowed    bool
	wasLocked  bool
	retryAfter time.Duration
	counts     map[string]int64
	thresholds []string
}

type adminMemoryCounter struct {
	count int64
	reset time.Time
}

type adminMemoryRateLimits struct {
	mu       sync.Mutex
	counters map[string]adminMemoryCounter
	locks    map[string]time.Time
}

// AdminOperationLimiter enforces operation-specific, per-human-admin limits.
// Redis is authoritative across replicas; the bounded in-process store keeps a
// single-node deployment protected when Redis is absent or briefly unavailable.
type AdminOperationLimiter struct {
	rdb    *redis.Client
	memory *adminMemoryRateLimits
	now    func() time.Time
	sink   AdminRateLimitEventSink
}

// NewAdminOperationLimiter builds the shared limiter used by both merchant
// action middleware and the root-only unlock endpoint.
func NewAdminOperationLimiter(rdb *redis.Client) *AdminOperationLimiter {
	return &AdminOperationLimiter{
		rdb: rdb,
		memory: &adminMemoryRateLimits{
			counters: make(map[string]adminMemoryCounter),
			locks:    make(map[string]time.Time),
		},
		now:  time.Now,
		sink: logAdminRateLimitEvent,
	}
}

// AdminRateLimitMW applies one operation policy after the merchant permission
// gate has pinned the effective principal. Non-human service credentials have
// no UserID and retain their existing admission controls.
func (l *AdminOperationLimiter) AdminRateLimitMW(operation AdminOperation) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			if l == nil {
				next(r)
				return
			}
			user, ok := r.UserContext()
			if !ok || strings.TrimSpace(user.UserID) == "" {
				next(r)
				return
			}

			userID, err := canonicalAdminUserID(user.UserID)
			if err != nil {
				r.AbortJSON(http.StatusInternalServerError, "administrative operation rate limit unavailable")
				return
			}
			decision := l.evaluate(r.Request.Context(), userID, operation)
			event := AdminRateLimitEvent{
				UserID:     userID,
				Operation:  operation,
				Counts:     decision.counts,
				RetryAfter: decision.retryAfter,
				RequestID:  r.RequestID(),
			}
			if mid, found := merchant.FromContext(r.Request.Context()); found {
				event.MerchantID = mid.String()
			}

			if !decision.allowed {
				if decision.wasLocked {
					event.Kind = "blocked"
				} else {
					event.Kind = "lockout"
				}
				l.emit(r.Request.Context(), event)
				retrySeconds := int64(math.Ceil(decision.retryAfter.Seconds()))
				if retrySeconds < 1 {
					retrySeconds = 1
				}
				r.SetHeader("Retry-After", fmt.Sprintf("%d", retrySeconds))
				r.APIError(&api.APIError{
					HTTPStatus: http.StatusTooManyRequests,
					Type:       api.ErrorTypeRateLimit,
					Code:       api.CodeRateLimitExceeded,
					Message:    "administrative operation rate limit exceeded",
				})
				return
			}

			event.Kind = "allowed"
			l.emit(r.Request.Context(), event)
			for _, window := range decision.thresholds {
				thresholdEvent := event
				thresholdEvent.Kind = "threshold"
				thresholdEvent.Counts = map[string]int64{window: decision.counts[window]}
				l.emit(r.Request.Context(), thresholdEvent)
			}
			next(r)
		}
	}
}

// Unlock clears the active lockout and current counters for userID. The actor
// is recorded separately because root operators may unlock another admin.
func (l *AdminOperationLimiter) Unlock(ctx context.Context, userID, actorID string) error {
	if l == nil {
		return nil
	}
	canonicalUserID, err := canonicalAdminUserID(userID)
	if err != nil {
		return fmt.Errorf("admin rate limit unlock: %w", err)
	}
	userID = canonicalUserID
	now := l.now().UTC()
	if l.rdb != nil {
		if err := l.unlockRedis(ctx, userID, now); err != nil {
			return fmt.Errorf("admin rate limit unlock: %w", err)
		}
	}
	l.unlockMemory(userID)
	l.emit(ctx, AdminRateLimitEvent{Kind: "unlocked", UserID: userID, ActorID: actorID})
	return nil
}

func (l *AdminOperationLimiter) evaluate(ctx context.Context, userID string, operation AdminOperation) adminRateLimitDecision {
	if canonicalUserID, err := canonicalAdminUserID(userID); err == nil {
		userID = canonicalUserID
	}
	windows := adminRateLimitPolicies[operation]
	if len(windows) == 0 {
		return adminRateLimitDecision{allowed: true}
	}
	now := l.now().UTC()
	if l.rdb != nil {
		decision, err := l.evaluateRedis(ctx, userID, operation, windows, now)
		if err == nil {
			return decision
		}
		log.WithError(err).WithFields(log.Fields{
			"admin_user_id": userID,
			"operation":     operation,
		}).Warn("admin rate limit redis error; falling back to in-memory limiter")
	}
	return l.evaluateMemory(userID, operation, windows, now)
}

func (l *AdminOperationLimiter) emit(ctx context.Context, event AdminRateLimitEvent) {
	if l != nil && l.sink != nil {
		l.sink(ctx, event)
	}
}

func logAdminRateLimitEvent(ctx context.Context, event AdminRateLimitEvent) {
	entry := log.WithContext(ctx).WithFields(log.Fields{
		"audit_event":   "admin_rate_limit." + event.Kind,
		"admin_user_id": event.UserID,
		"actor_user_id": event.ActorID,
		"merchant_id":   event.MerchantID,
		"operation":     event.Operation,
		"counts":        event.Counts,
		"retry_after_s": int64(math.Ceil(event.RetryAfter.Seconds())),
		"request_id":    event.RequestID,
	})
	switch event.Kind {
	case "lockout":
		entry.Error("admin operation lockout activated")
	case "blocked", "threshold":
		entry.Warn("admin operation rate limit alert")
	default:
		entry.Info("admin operation rate limit audit")
	}
}

func (l *AdminOperationLimiter) evaluateMemory(userID string, operation AdminOperation, windows []adminRateLimitWindow, now time.Time) adminRateLimitDecision {
	l.memory.mu.Lock()
	defer l.memory.mu.Unlock()
	l.memory.prune(now)

	if until := l.memory.locks[userID]; until.After(now) {
		return adminRateLimitDecision{allowed: false, wasLocked: true, retryAfter: until.Sub(now)}
	}
	delete(l.memory.locks, userID)

	decision := adminRateLimitDecision{allowed: true, counts: make(map[string]int64, len(windows))}
	breached := false
	for _, window := range windows {
		key := adminRateLimitCounterKey(userID, operation, window, now)
		counter := l.memory.counters[key]
		if !counter.reset.After(now) {
			counter = adminMemoryCounter{reset: nextAdminRateLimitWindow(now, window.duration)}
		}
		counter.count++
		l.memory.counters[key] = counter
		decision.counts[window.name] = counter.count
		if counter.count == adminRateLimitAlertThreshold(window.limit) {
			decision.thresholds = append(decision.thresholds, window.name)
		}
		if counter.count > window.limit {
			breached = true
		}
	}
	if breached {
		until := now.Add(adminLockoutDuration)
		l.memory.locks[userID] = until
		decision.allowed = false
		decision.retryAfter = adminLockoutDuration
	}
	return decision
}

func (m *adminMemoryRateLimits) prune(now time.Time) {
	for userID, until := range m.locks {
		if !until.After(now) {
			delete(m.locks, userID)
		}
	}
	for key, counter := range m.counters {
		if !counter.reset.After(now) {
			delete(m.counters, key)
		}
	}
	for len(m.counters) >= maxInMemoryRateLimitCounters {
		var oldestKey string
		var oldestReset time.Time
		for key, counter := range m.counters {
			if oldestKey == "" || counter.reset.Before(oldestReset) {
				oldestKey = key
				oldestReset = counter.reset
			}
		}
		if oldestKey == "" {
			return
		}
		delete(m.counters, oldestKey)
	}
}

func (l *AdminOperationLimiter) unlockMemory(userID string) {
	l.memory.mu.Lock()
	defer l.memory.mu.Unlock()
	delete(l.memory.locks, userID)
	prefix := "admin-rl:{" + userID + "}:"
	for key := range l.memory.counters {
		if strings.HasPrefix(key, prefix) {
			delete(l.memory.counters, key)
		}
	}
}

func adminRateLimitAlertThreshold(limit int64) int64 {
	return int64(math.Ceil(float64(limit) * 0.8))
}

func nextAdminRateLimitWindow(now time.Time, duration time.Duration) time.Time {
	seconds := int64(duration / time.Second)
	return time.Unix(((now.Unix()/seconds)+1)*seconds, 0).UTC()
}

func adminRateLimitCounterKey(userID string, operation AdminOperation, window adminRateLimitWindow, now time.Time) string {
	start := now.Unix() / int64(window.duration/time.Second)
	return fmt.Sprintf("admin-rl:{%s}:%s:%s:%d", userID, operation, window.name, start)
}

func adminRateLimitLockKey(userID string) string {
	return fmt.Sprintf("admin-rl:{%s}:lock", userID)
}

func canonicalAdminUserID(userID string) (string, error) {
	id, err := uuid.Parse(strings.TrimSpace(userID))
	if err != nil {
		return "", fmt.Errorf("invalid user id")
	}
	return id.String(), nil
}

var adminRateLimitScript = redis.NewScript(`
local lock_ttl = redis.call("TTL", KEYS[1])
if lock_ttl > 0 then
  local locked = {0, lock_ttl, 1}
  for i = 2, #KEYS do table.insert(locked, 0) end
  return locked
end

local counts = {}
local breached = 0
for i = 2, #KEYS do
  local arg = ((i - 2) * 2) + 2
  local count = redis.call("INCR", KEYS[i])
  if count == 1 then redis.call("EXPIRE", KEYS[i], ARGV[arg]) end
  table.insert(counts, count)
  if count > tonumber(ARGV[arg + 1]) then breached = 1 end
end

if breached == 1 then
  redis.call("SET", KEYS[1], "1", "EX", ARGV[1])
end

local result = {1, 0, 0}
if breached == 1 then result = {0, tonumber(ARGV[1]), 0} end
for _, count in ipairs(counts) do table.insert(result, count) end
return result
`)

func (l *AdminOperationLimiter) evaluateRedis(ctx context.Context, userID string, operation AdminOperation, windows []adminRateLimitWindow, now time.Time) (adminRateLimitDecision, error) {
	keys := make([]string, 1, len(windows)+1)
	keys[0] = adminRateLimitLockKey(userID)
	args := make([]any, 1, (len(windows)*2)+1)
	args[0] = int64(adminLockoutDuration / time.Second)
	for _, window := range windows {
		keys = append(keys, adminRateLimitCounterKey(userID, operation, window, now))
		ttl := int64(math.Ceil(nextAdminRateLimitWindow(now, window.duration).Sub(now).Seconds()))
		if ttl < 1 {
			ttl = 1
		}
		args = append(args, ttl, window.limit)
	}

	values, err := adminRateLimitScript.Run(ctx, l.rdb, keys, args...).Int64Slice()
	if err != nil {
		return adminRateLimitDecision{}, err
	}
	if len(values) != len(windows)+3 {
		return adminRateLimitDecision{}, fmt.Errorf("unexpected redis result length %d", len(values))
	}
	decision := adminRateLimitDecision{
		allowed:    values[0] == 1,
		wasLocked:  values[2] == 1,
		retryAfter: time.Duration(values[1]) * time.Second,
		counts:     make(map[string]int64, len(windows)),
	}
	for i, window := range windows {
		count := values[i+3]
		decision.counts[window.name] = count
		if count == adminRateLimitAlertThreshold(window.limit) {
			decision.thresholds = append(decision.thresholds, window.name)
		}
	}
	return decision, nil
}

func (l *AdminOperationLimiter) unlockRedis(ctx context.Context, userID string, now time.Time) error {
	keys := []string{adminRateLimitLockKey(userID)}
	for operation, windows := range adminRateLimitPolicies {
		for _, window := range windows {
			keys = append(keys, adminRateLimitCounterKey(userID, operation, window, now))
		}
	}
	return l.rdb.Del(ctx, keys...).Err()
}
