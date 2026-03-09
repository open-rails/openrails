package audit

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/internal/db/models"
)

// P-E-1: Completed one-off payment missing entitlements
type CheckCompletedPaymentMissingEntitlements struct{}

func (c *CheckCompletedPaymentMissingEntitlements) ID() string { return "P-E-1" }
func (c *CheckCompletedPaymentMissingEntitlements) Name() string {
	return "completed_payment_missing_entitlements"
}
func (c *CheckCompletedPaymentMissingEntitlements) Category() string   { return "payment_entitlement" }
func (c *CheckCompletedPaymentMissingEntitlements) Severity() Severity { return SeverityHigh }

func (c *CheckCompletedPaymentMissingEntitlements) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	// Get all one-off payments (subscription_id IS NULL) with their product
	var payments []models.Payment
	q := db.NewSelect().
		Model(&payments).
		Relation("Price").
		Relation("Price.Product").
		Where("purch.subscription_id IS NULL").
		Where("purch.amount > 0") // Exclude refunds (negative amounts)

	if opts.UserID != "" {
		q = q.Where("purch.user_id = ?", opts.UserID)
	}
	if opts.Since != nil {
		q = q.Where("purch.created_at >= ?", opts.Since)
	}

	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("query one-off payments: %w", err)
	}

	for _, payment := range payments {
		if payment.Price == nil || payment.Price.Product == nil {
			continue
		}

		product := payment.Price.Product
		if product.EntitlementsSpec == nil {
			continue
		}

		// Check if any entitlement was granted from this payment
		var count int
		count, err := db.NewSelect().
			Model((*models.Entitlement)(nil)).
			Where("source_type = ?", models.EntitlementSourceOneOff).
			Where("source_id = ?", payment.ID).
			Count(ctx)

		if err != nil {
			return nil, fmt.Errorf("check entitlements for payment %s: %w", payment.ID, err)
		}

		if count == 0 {
			findings = append(findings, Finding{
				CheckID:        c.ID(),
				CheckName:      c.Name(),
				Severity:       c.Severity(),
				EntityType:     EntityPayment,
				EntityID:       payment.ID,
				UserID:         payment.UserID,
				Description:    fmt.Sprintf("Completed one-off payment %s for product '%s' has no entitlements granted", payment.ID, product.Slug),
				Recommendation: fmt.Sprintf("Grant entitlements from product.entitlements_spec with source_type=one_off, source_id=%s", payment.ID),
				AutoFixable:    true,
				Details: map[string]any{
					"product_slug": product.Slug,
					"product_id":   product.ID,
					"amount":       payment.Amount,
					"purchased_at": payment.PurchasedAt,
				},
			})
		}
	}

	return findings, nil
}

// P-E-2: Orphan one-off entitlements
type CheckOrphanOneOffEntitlements struct{}

func (c *CheckOrphanOneOffEntitlements) ID() string         { return "P-E-2" }
func (c *CheckOrphanOneOffEntitlements) Name() string       { return "orphan_one_off_entitlements" }
func (c *CheckOrphanOneOffEntitlements) Category() string   { return "payment_entitlement" }
func (c *CheckOrphanOneOffEntitlements) Severity() Severity { return SeverityMedium }

func (c *CheckOrphanOneOffEntitlements) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find entitlements where source_type=one_off but payment doesn't exist or has amount <= 0 (refund)
	type result struct {
		EntitlementID   uuid.UUID `bun:"ent_id"`
		UserID          string    `bun:"user_id"`
		EntitlementName string    `bun:"entitlement"`
		SourceID        uuid.UUID `bun:"source_id"`
		PaymentExists   bool      `bun:"payment_exists"`
		PaymentAmount   *int64    `bun:"payment_amount"`
	}

	var results []result
	err := db.NewRaw(`
		SELECT
			ent.id as ent_id,
			ent.user_id,
			ent.entitlement,
			ent.source_id,
			CASE WHEN purch.id IS NOT NULL THEN true ELSE false END as payment_exists,
			purch.amount as payment_amount
		FROM billing.entitlements ent
		LEFT JOIN billing.payments purch ON ent.source_id = purch.id
		WHERE ent.source_type = 'one_off'
		  AND ent.revoked_at IS NULL
		  AND ent.deleted_at IS NULL
		  AND (ent.end_at IS NULL OR ent.end_at > NOW())
		  AND (purch.id IS NULL OR purch.amount <= 0)
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query orphan one-off entitlements: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		reason := "payment not found"
		if r.PaymentExists && r.PaymentAmount != nil && *r.PaymentAmount <= 0 {
			reason = "payment was refunded"
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityEntitlement,
			EntityID:       r.EntitlementID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Entitlement '%s' references payment %s which is invalid (%s)", r.EntitlementName, r.SourceID, reason),
			Recommendation: "Revoke entitlement",
			AutoFixable:    true,
			Details: map[string]any{
				"payment_id":     r.SourceID,
				"payment_exists": r.PaymentExists,
				"payment_amount": r.PaymentAmount,
			},
		})
	}

	return findings, nil
}

// P-E-3: Refunded payment with active entitlements
type CheckRefundedPaymentActiveEntitlements struct{}

func (c *CheckRefundedPaymentActiveEntitlements) ID() string { return "P-E-3" }
func (c *CheckRefundedPaymentActiveEntitlements) Name() string {
	return "refunded_payment_active_entitlements"
}
func (c *CheckRefundedPaymentActiveEntitlements) Category() string   { return "payment_entitlement" }
func (c *CheckRefundedPaymentActiveEntitlements) Severity() Severity { return SeverityHigh }

func (c *CheckRefundedPaymentActiveEntitlements) Run(ctx context.Context, db bun.IDB, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find payments where total refunds >= original amount but entitlements still active
	type result struct {
		PaymentID       uuid.UUID `bun:"payment_id"`
		UserID          string    `bun:"user_id"`
		OriginalAmount  int64     `bun:"original_amount"`
		RefundedAmount  int64     `bun:"refunded_amount"`
		EntitlementID   uuid.UUID `bun:"ent_id"`
		EntitlementName string    `bun:"entitlement"`
	}

	var results []result
	err := db.NewRaw(`
		WITH refund_totals AS (
			SELECT
				refunded_payment_id,
				SUM(ABS(amount)) as total_refunded
			FROM billing.payments
			WHERE refunded_payment_id IS NOT NULL
			GROUP BY refunded_payment_id
		)
		SELECT
			purch.id as payment_id,
			purch.user_id,
			purch.amount as original_amount,
			COALESCE(rt.total_refunded, 0) as refunded_amount,
			ent.id as ent_id,
			ent.entitlement
		FROM billing.payments purch
		LEFT JOIN refund_totals rt ON rt.refunded_payment_id = purch.id
		INNER JOIN billing.entitlements ent ON ent.source_id = purch.id
		WHERE purch.subscription_id IS NULL
		  AND purch.amount > 0
		  AND COALESCE(rt.total_refunded, 0) >= purch.amount
		  AND ent.source_type = 'one_off'
		  AND ent.revoked_at IS NULL
		  AND ent.deleted_at IS NULL
		  AND (ent.end_at IS NULL OR ent.end_at > NOW())
	`).Scan(ctx, &results)

	if err != nil {
		return nil, fmt.Errorf("query refunded payment entitlements: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityEntitlement,
			EntityID:       r.EntitlementID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Payment %s was fully refunded (%s of %s) but entitlement '%s' is still active", r.PaymentID, moneyutil.FormatUSD(r.RefundedAmount), moneyutil.FormatUSD(r.OriginalAmount), r.EntitlementName),
			Recommendation: "Revoke entitlement with reason='refund'",
			AutoFixable:    true,
			Details: map[string]any{
				"payment_id":      r.PaymentID,
				"original_amount": r.OriginalAmount,
				"refunded_amount": r.RefundedAmount,
			},
		})
	}

	return findings, nil
}
