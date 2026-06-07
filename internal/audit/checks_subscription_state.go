package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// SS-1: Active subscription past period end
type CheckActiveSubscriptionPastPeriodEnd struct{}

func (c *CheckActiveSubscriptionPastPeriodEnd) ID() string { return "SS-1" }
func (c *CheckActiveSubscriptionPastPeriodEnd) Name() string {
	return "active_subscription_past_period_end"
}
func (c *CheckActiveSubscriptionPastPeriodEnd) Category() string   { return "subscription_state" }
func (c *CheckActiveSubscriptionPastPeriodEnd) Severity() Severity { return SeverityHigh }

func (c *CheckActiveSubscriptionPastPeriodEnd) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		ID           uuid.UUID `bun:"id"`
		UserID       string    `bun:"user_id"`
		PeriodEndsAt time.Time `bun:"current_period_ends_at"`
	}

	var results []result
	q := db.NewRaw(`
		SELECT id, tenant_subject_id::text AS user_id, current_period_ends_at
		FROM billing.subscriptions
		WHERE status = 'active'
		  AND current_period_ends_at < NOW()
	`)

	if err := db.NewRaw(q.String()).Scan(ctx, &results); err != nil {
		return nil, fmt.Errorf("query past period end subscriptions: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Subscription is active but period ended at %s", r.PeriodEndsAt.Format(time.RFC3339)),
			Recommendation: "Transition to past_due and attempt rebill, or cancel if grace period exceeded",
			AutoFixable:    true,
			Details: map[string]any{
				"period_ends_at": r.PeriodEndsAt,
				"hours_overdue":  time.Since(r.PeriodEndsAt).Hours(),
			},
		})
	}

	return findings, nil
}

// SS-2: Cancelled without metadata
type CheckCancelledWithoutMetadata struct{}

func (c *CheckCancelledWithoutMetadata) ID() string         { return "SS-2" }
func (c *CheckCancelledWithoutMetadata) Name() string       { return "cancelled_without_metadata" }
func (c *CheckCancelledWithoutMetadata) Category() string   { return "subscription_state" }
func (c *CheckCancelledWithoutMetadata) Severity() Severity { return SeverityMedium }

func (c *CheckCancelledWithoutMetadata) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		ID          uuid.UUID  `bun:"id"`
		UserID      string     `bun:"user_id"`
		CancelledAt *time.Time `bun:"cancelled_at"`
		CancelType  *string    `bun:"cancel_type"`
		UpdatedAt   time.Time  `bun:"updated_at"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT id, tenant_subject_id::text AS user_id, cancelled_at, cancel_type, updated_at
		FROM billing.subscriptions
		WHERE status = 'cancelled'
		  AND (cancelled_at IS NULL OR cancel_type IS NULL)
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query cancelled without metadata: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		missing := []string{}
		if r.CancelledAt == nil {
			missing = append(missing, "cancelled_at")
		}
		if r.CancelType == nil {
			missing = append(missing, "cancel_type")
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Cancelled subscription missing: %v", missing),
			Recommendation: "Set cancelled_at=updated_at and cancel_type='unknown'",
			AutoFixable:    true,
			Details: map[string]any{
				"missing_fields": missing,
				"updated_at":     r.UpdatedAt,
			},
		})
	}

	return findings, nil
}

// SS-3: Past due without retry scheduled
type CheckPastDueWithoutRetry struct{}

func (c *CheckPastDueWithoutRetry) ID() string         { return "SS-3" }
func (c *CheckPastDueWithoutRetry) Name() string       { return "past_due_without_retry" }
func (c *CheckPastDueWithoutRetry) Category() string   { return "subscription_state" }
func (c *CheckPastDueWithoutRetry) Severity() Severity { return SeverityMedium }

func (c *CheckPastDueWithoutRetry) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		ID            uuid.UUID `bun:"id"`
		UserID        string    `bun:"user_id"`
		RetryAttempts *int      `bun:"retry_attempts"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT id, tenant_subject_id::text AS user_id, retry_attempts
		FROM billing.subscriptions
		WHERE status = 'past_due'
		  AND next_retry_at IS NULL
		  AND COALESCE(retry_attempts, 0) < 5
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query past due without retry: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		attempts := 0
		if r.RetryAttempts != nil {
			attempts = *r.RetryAttempts
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Past-due subscription has no next_retry_at scheduled (attempts: %d/5)", attempts),
			Recommendation: "Set next_retry_at to schedule next dunning attempt",
			AutoFixable:    true,
			Details: map[string]any{
				"retry_attempts": attempts,
			},
		})
	}

	return findings, nil
}

// SS-4: Invalid period dates
type CheckInvalidPeriodDates struct{}

func (c *CheckInvalidPeriodDates) ID() string         { return "SS-4" }
func (c *CheckInvalidPeriodDates) Name() string       { return "invalid_period_dates" }
func (c *CheckInvalidPeriodDates) Category() string   { return "subscription_state" }
func (c *CheckInvalidPeriodDates) Severity() Severity { return SeverityHigh }

func (c *CheckInvalidPeriodDates) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		ID             uuid.UUID `bun:"id"`
		UserID         string    `bun:"user_id"`
		PeriodStartsAt time.Time `bun:"current_period_starts_at"`
		PeriodEndsAt   time.Time `bun:"current_period_ends_at"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT id, tenant_subject_id::text AS user_id, current_period_starts_at, current_period_ends_at
		FROM billing.subscriptions
		WHERE current_period_starts_at IS NOT NULL
		  AND current_period_ends_at IS NOT NULL
		  AND current_period_starts_at >= current_period_ends_at
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query invalid period dates: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Period start (%s) >= period end (%s)", r.PeriodStartsAt.Format(time.RFC3339), r.PeriodEndsAt.Format(time.RFC3339)),
			Recommendation: "MANUAL REVIEW - Data corruption detected",
			AutoFixable:    false,
			Details: map[string]any{
				"period_starts_at": r.PeriodStartsAt,
				"period_ends_at":   r.PeriodEndsAt,
			},
		})
	}

	return findings, nil
}

// SS-5: Ended before cancelled
type CheckEndedBeforeCancelled struct{}

func (c *CheckEndedBeforeCancelled) ID() string         { return "SS-5" }
func (c *CheckEndedBeforeCancelled) Name() string       { return "ended_before_cancelled" }
func (c *CheckEndedBeforeCancelled) Category() string   { return "subscription_state" }
func (c *CheckEndedBeforeCancelled) Severity() Severity { return SeverityLow }

func (c *CheckEndedBeforeCancelled) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	type result struct {
		ID          uuid.UUID `bun:"id"`
		UserID      string    `bun:"user_id"`
		EndedAt     time.Time `bun:"ended_at"`
		CancelledAt time.Time `bun:"cancelled_at"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT id, tenant_subject_id::text AS user_id, ended_at, cancelled_at
		FROM billing.subscriptions
		WHERE ended_at IS NOT NULL
		  AND cancelled_at IS NOT NULL
		  AND ended_at < cancelled_at
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query ended before cancelled: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.ID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("ended_at (%s) < cancelled_at (%s) - temporal ordering violation", r.EndedAt.Format(time.RFC3339), r.CancelledAt.Format(time.RFC3339)),
			Recommendation: "Set ended_at = cancelled_at",
			AutoFixable:    true,
			Details: map[string]any{
				"ended_at":     r.EndedAt,
				"cancelled_at": r.CancelledAt,
			},
		})
	}

	return findings, nil
}
