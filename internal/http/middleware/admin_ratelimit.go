package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/billingauth"

	"github.com/open-rails/openrails/billing"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/cadence"
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

// AdminOperationLimiter enforces operation-specific, per-human-admin limits,
// counted in the process's abuse state.
type AdminOperationLimiter struct {
	state *abusestate.Store
	sink  AdminRateLimitEventSink
}

// NewAdminOperationLimiter builds the shared limiter used by both merchant
// action middleware and the root-only unlock endpoint.
func NewAdminOperationLimiter(state *abusestate.Store) *AdminOperationLimiter {
	return &AdminOperationLimiter{state: state, sink: logAdminRateLimitEvent}
}

// AdminRateLimitMW applies one operation policy after the route gate has
// admitted its staff member, keyed on the invoker: who actually acts,
// whatever credential they signed in with. A route the gate admitted nobody
// to is refused.
func (l *AdminOperationLimiter) AdminRateLimitMW(operation AdminOperation) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			if l == nil {
				next(r)
				return
			}
			staff, ok := r.Staff()
			if !ok {
				r.AbortCode(billing.CodeAuthenticationRequired, "")
				return
			}
			// An application acting itself keeps its own admission controls;
			// a user, or anyone acting on a subject's behalf, is metered as
			// its invoker.
			if staff.SubjectKind == billingauth.SubjectApplication && billingauth.SelfActing(staff.Identity) {
				next(r)
				return
			}
			userID := adminActorKey(staff.Invoker.Issuer, staff.Invoker.ID)
			decision := l.evaluate(r.Request.Context(), userID, operation, requestItems(r))
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
	// Memory is cleared regardless; an error is a Redis that was not.
	err = l.state.Release(ctx, adminRateLimitLockKey(userID))
	for operation, windows := range adminRateLimitPolicies {
		for _, window := range windows {
			if e := l.state.ResetCounts(ctx, window.duration, adminRateLimitCounterKey(userID, operation, window)); err == nil {
				err = e
			}
		}
	}
	if err != nil {
		return fmt.Errorf("admin rate limit unlock: %w", err)
	}
	l.emit(ctx, AdminRateLimitEvent{Kind: "unlocked", UserID: userID, ActorID: actorID})
	return nil
}

// requestItems is how many operations a request carries: the length of its
// JSON body's "items" batch, else one. Limits count operations, so a batch
// never buys more than the same calls one at a time.
func requestItems(r *request.Request) int64 {
	var batch struct {
		Items []json.RawMessage `json:"items"`
	}
	if r.PeekJSON(&batch) != nil || len(batch.Items) == 0 {
		return 1
	}
	return int64(len(batch.Items))
}

func (l *AdminOperationLimiter) evaluate(ctx context.Context, userID string, operation AdminOperation, weight int64) adminRateLimitDecision {
	if canonicalUserID, err := canonicalAdminUserID(userID); err == nil {
		userID = canonicalUserID
	}
	windows := adminRateLimitPolicies[operation]
	if len(windows) == 0 {
		return adminRateLimitDecision{allowed: true}
	}
	lock := adminRateLimitLockKey(userID)
	if left := l.state.Held(ctx, lock); left > 0 {
		return adminRateLimitDecision{wasLocked: true, retryAfter: left}
	}
	decision := adminRateLimitDecision{allowed: true, counts: make(map[string]int64, len(windows))}
	breached := false
	for _, window := range windows {
		count, _ := l.state.Count(ctx, adminRateLimitCounterKey(userID, operation, window), weight, window.duration)
		decision.counts[window.name] = count
		if crossed(count, weight, adminRateLimitAlertThreshold(window.limit)) {
			decision.thresholds = append(decision.thresholds, window.name)
		}
		breached = breached || count > window.limit
	}
	if breached {
		l.state.Hold(ctx, lock, adminLockoutDuration)
		decision.allowed, decision.retryAfter = false, adminLockoutDuration
	}
	return decision
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
		"retry_after":   cadence.FormatDuration(time.Duration(math.Ceil(event.RetryAfter.Seconds())) * time.Second),
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

// crossed reports whether adding weight brought count to threshold.
func crossed(count, weight, threshold int64) bool {
	return count >= threshold && count-weight < threshold
}

func adminRateLimitAlertThreshold(limit int64) int64 {
	return int64(math.Ceil(float64(limit) * 0.8))
}

func adminRateLimitCounterKey(userID string, operation AdminOperation, window adminRateLimitWindow) string {
	return fmt.Sprintf("admin-rl:{%s}:%s:%s", userID, operation, window.name)
}

func adminRateLimitLockKey(userID string) string {
	return fmt.Sprintf("admin-rl:{%s}:lock", userID)
}

// adminActorKey is an invoker's limiter key: its UUID, or for another
// issuer's opaque id, the pair.
func adminActorKey(issuer, subject string) string {
	if id, err := canonicalAdminUserID(subject); err == nil {
		return id
	}
	return issuer + "|" + subject
}

func canonicalAdminUserID(userID string) (string, error) {
	id, err := uuid.Parse(strings.TrimSpace(userID))
	if err != nil {
		return "", fmt.Errorf("invalid user id")
	}
	return id.String(), nil
}
