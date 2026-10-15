package destructive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// UserActionRateError is a temporary merchant-wide refusal, not an incident
// requiring a manual clear. It never authorizes a queued retry.
type UserActionRateError struct{ RetryAfterSeconds int64 }

type userActionRateEvidence struct {
	Window                 string `json:"window"`
	HourlyCustomers        int64  `json:"hourly_customers"`
	MaximumHourlyCustomers int64  `json:"maximum_hourly_customers"`
	DailyCustomers         int64  `json:"daily_customers"`
	MaximumDailyCustomers  int64  `json:"maximum_daily_customers"`
	RetryAfterSeconds      int64  `json:"retry_after_seconds"`
}

func (e *UserActionRateError) Error() string {
	return "too many card removals or subscription cancellations; try again later"
}
func (e *UserActionRateError) Is(target error) bool { return target == billing.ErrDenied }
func (e *UserActionRateError) ErrorCode() string    { return billing.CodeRateLimitExceeded }

// UserActionLimiter reserves one logical customer action across every process
// serving the merchant. It is independent of optional IP/user HTTP limits.
type UserActionLimiter struct {
	db     *db.DB
	policy config.UserActionRateLimitConfig
	err    error
}

func NewUserActionLimiter(database *db.DB, policy config.UserActionRateLimitConfig) *UserActionLimiter {
	normalized, err := config.NormalizeUserActionRateLimit(policy)
	return &UserActionLimiter{db: database, policy: normalized, err: err}
}

func (l *UserActionLimiter) limit(customers int64) int64 {
	return max(l.policy.MinimumCustomersPerDay, ceil(customers, l.policy.PaidCustomersPerDailyCustomer))
}

// Reserve must run after ownership and relevance checks, before the first
// mutation, outside a caller transaction. It commits only a rate reservation;
// the operation retains its own validation, idempotency and provider posture.
// A retry of this logical action reuses its reservation for 24 hours. Completed
// actions remain no-ops through their domain records after pruning; an older
// uncompleted retry is admitted against the current rolling day again.
func (l *UserActionLimiter) Reserve(ctx context.Context, kind Kind, key string, customerID uuid.UUID) error {
	if l == nil || l.db == nil || l.db.DataPool() == nil {
		return errors.New("user action rate limiter unavailable")
	}
	if l.err != nil {
		return l.err
	}
	if customerID == uuid.Nil || (kind != DeletePaymentMethod && kind != CancelSubscription) || strings.TrimSpace(key) == "" || len(key) > 512 {
		return errors.New("invalid user action rate reservation")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	var refusal *UserActionRateError
	err = l.db.DataPool().CommittedMerchantTx(ctx, mid, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.LockUserDestructiveReservations(ctx, mid.UUID()); err != nil {
			return err
		}
		now, err := q.UserDestructiveClock(ctx)
		if err != nil {
			return err
		}
		prior, err := q.UserDestructiveReservation(ctx, gen.UserDestructiveReservationParams{MerchantID: mid.UUID(), ActionKey: key})
		if err == nil {
			if prior.CustomerID != customerID || prior.Kind != string(kind) {
				return errors.New("user action rate reservation identity mismatch")
			}
			if prior.ReservedAt.After(now.Add(-24 * time.Hour)) {
				return nil
			}
			if err := q.DeleteExpiredUserDestructiveReservation(ctx, gen.DeleteExpiredUserDestructiveReservationParams{MerchantID: mid.UUID(), ActionKey: key, DaySince: now.Add(-24 * time.Hour)}); err != nil {
				return err
			}
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		customers, err := q.UserDestructivePaidCustomers(ctx, gen.UserDestructivePaidCustomersParams{MerchantID: mid.UUID(), At: now, MonthSince: now.Add(-30 * 24 * time.Hour)})
		if err != nil {
			return err
		}
		limit := l.limit(customers)
		hourlyLimit := max(l.policy.MinimumCustomersPerHour, ceil(customers, l.policy.PaidCustomersPerHourlyCustomer))
		window, err := q.UserDestructiveWindow(ctx, gen.UserDestructiveWindowParams{MerchantID: mid.UUID(), DaySince: now.Add(-24 * time.Hour), HourSince: now.Add(-time.Hour), CustomerID: customerID, DayLimit: limit, HourLimit: hourlyLimit})
		if err != nil {
			return err
		}

		wait := time.Duration(0)
		if !window.CustomerCounted && window.AffectedCustomers >= limit {
			wait = window.OldestCustomer.Add(24 * time.Hour).Sub(now)
		}
		if !window.CustomerCountedHour && window.HourlyCustomers >= hourlyLimit {
			hourlyWait := window.OldestHourCustomer.Add(time.Hour).Sub(now)
			wait = max(wait, hourlyWait)
		}
		if wait > 0 {
			refusal = &UserActionRateError{RetryAfterSeconds: max(1, int64((wait+time.Second-1)/time.Second))}
			evidence, err := json.Marshal(userActionRateEvidence{
				Window: "rolling hour/day", HourlyCustomers: window.HourlyCustomers,
				MaximumHourlyCustomers: hourlyLimit, DailyCustomers: window.AffectedCustomers,
				MaximumDailyCustomers: limit, RetryAfterSeconds: refusal.RetryAfterSeconds,
			})
			if err != nil {
				return err
			}
			action := "Review unusual customer card-removal or cancellation volume. This temporary limit clears as the rolling windows expire."
			_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: mid.UUID(), FindingType: "life.customer_action_rate.exceeded", SubjectKey: "merchant", Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence})
			return err
		}
		return q.ReserveUserDestructiveAction(ctx, gen.ReserveUserDestructiveActionParams{MerchantID: mid.UUID(), ActionKey: key, Kind: string(kind), At: now, CustomerID: customerID})
	})
	if err != nil {
		return fmt.Errorf("reserve customer action: %w", err)
	}
	if refusal != nil {
		return refusal
	}
	return nil
}

// PruneUserActionReservations bounds this operational table. It does not
// delete or reset the canonical card, subscription or provider-operation log.
func PruneUserActionReservations(ctx context.Context, database *db.DB, batch int32) (int64, error) {
	if database == nil || batch < 1 {
		return 0, errors.New("invalid user action reservation cleanup")
	}
	return database.GenDirectory().PruneUserDestructiveReservations(ctx, batch)
}
