package audit

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/db/gen"
)

// PM-1: Active subscription with payment method that has a failure reason
type CheckActiveSubscriptionFailedPaymentMethod struct{}

func (c *CheckActiveSubscriptionFailedPaymentMethod) ID() string { return "PM-1" }
func (c *CheckActiveSubscriptionFailedPaymentMethod) Name() string {
	return "active_subscription_failed_payment_method"
}
func (c *CheckActiveSubscriptionFailedPaymentMethod) Category() string   { return "payment_method" }
func (c *CheckActiveSubscriptionFailedPaymentMethod) Severity() Severity { return SeverityHigh }

func (c *CheckActiveSubscriptionFailedPaymentMethod) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditActiveSubscriptionFailedPaymentMethod(ctx)
	if err != nil {
		return nil, fmt.Errorf("query active sub failed pm: %w", err)
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
			EntityID:       r.SubID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Active subscription uses payment method with failure reason: %s", r.FailureReason),
			Recommendation: "Prompt user to update payment method or transition subscription to past_due",
			AutoFixable:    false,
			Details: map[string]any{
				"payment_method_id": r.PmID,
				"failure_reason":    r.FailureReason,
			},
		})
	}

	return findings, nil
}

// PM-2: Expired card with active subscription
type CheckExpiredCardActiveSubscription struct{}

func (c *CheckExpiredCardActiveSubscription) ID() string         { return "PM-2" }
func (c *CheckExpiredCardActiveSubscription) Name() string       { return "expired_card_active_subscription" }
func (c *CheckExpiredCardActiveSubscription) Category() string   { return "payment_method" }
func (c *CheckExpiredCardActiveSubscription) Severity() Severity { return SeverityMedium }

func (c *CheckExpiredCardActiveSubscription) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// We compare expiry_date (MM/YY) with current month/year
	results, err := q.AuditExpiredCardActiveSubscription(ctx)
	if err != nil {
		return nil, fmt.Errorf("query expired cards: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		cardDesc := "card"
		if r.CardType != nil && r.LastFour != nil {
			cardDesc = fmt.Sprintf("%s ending in %s", *r.CardType, *r.LastFour)
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityPaymentMethod,
			EntityID:       r.PmID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Active subscription uses %s which expired %s", cardDesc, r.ExpiryDate),
			Recommendation: "Notify user to update payment method before next rebill fails",
			AutoFixable:    false,
			Details: map[string]any{
				"subscription_id": r.SubID,
				"expiry_date":     r.ExpiryDate,
				"last_four":       r.LastFour,
				"card_type":       r.CardType,
			},
		})
	}

	return findings, nil
}

// PM-3: Orphan payment method reference
type CheckOrphanPaymentMethodReference struct{}

func (c *CheckOrphanPaymentMethodReference) ID() string         { return "PM-3" }
func (c *CheckOrphanPaymentMethodReference) Name() string       { return "orphan_payment_method_reference" }
func (c *CheckOrphanPaymentMethodReference) Category() string   { return "payment_method" }
func (c *CheckOrphanPaymentMethodReference) Severity() Severity { return SeverityHigh }

func (c *CheckOrphanPaymentMethodReference) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditOrphanPaymentMethodReference(ctx)
	if err != nil {
		return nil, fmt.Errorf("query orphan pm references: %w", err)
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
			EntityID:       r.SubID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Subscription references non-existent payment_method_id %s", r.PaymentMethodID),
			Recommendation: "Clear payment_method_id and notify user to add new payment method",
			AutoFixable:    true,
			Details: map[string]any{
				"missing_payment_method_id": r.PaymentMethodID,
			},
		})
	}

	return findings, nil
}

// PM-4: Processor mismatch
type CheckProcessorMismatch struct{}

func (c *CheckProcessorMismatch) ID() string         { return "PM-4" }
func (c *CheckProcessorMismatch) Name() string       { return "processor_mismatch" }
func (c *CheckProcessorMismatch) Category() string   { return "payment_method" }
func (c *CheckProcessorMismatch) Severity() Severity { return SeverityHigh }

func (c *CheckProcessorMismatch) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditProcessorMismatch(ctx)
	if err != nil {
		return nil, fmt.Errorf("query processor mismatch: %w", err)
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
			EntityID:       r.SubID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Subscription processor (%s) != payment method processor (%s)", r.SubProcessor, r.PmProcessor),
			Recommendation: "MANUAL REVIEW - Configuration error",
			AutoFixable:    false,
			Details: map[string]any{
				"subscription_processor":   r.SubProcessor,
				"payment_method_processor": r.PmProcessor,
				"payment_method_id":        r.PmID,
			},
		})
	}

	return findings, nil
}
