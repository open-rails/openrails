package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// P-E-1: Completed one-off payment missing entitlements
type CheckCompletedPaymentMissingEntitlements struct{}

func (c *CheckCompletedPaymentMissingEntitlements) ID() string { return "P-E-1" }
func (c *CheckCompletedPaymentMissingEntitlements) Name() string {
	return "completed_payment_missing_entitlements"
}
func (c *CheckCompletedPaymentMissingEntitlements) Category() string   { return "payment_entitlement" }
func (c *CheckCompletedPaymentMissingEntitlements) Severity() Severity { return SeverityHigh }

func (c *CheckCompletedPaymentMissingEntitlements) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Get all one-off payments (subscription_id IS NULL) with their product
	var params gen.AuditOneOffPaymentsWithSpecParams
	if opts.UserID != "" {
		tsid, err := resolveTenantSubjectID(ctx, q, opts.UserID)
		if err != nil {
			return nil, err
		}
		params.TenantSubjectID = &tsid
	}
	if opts.Since != nil {
		params.Since = opts.Since
	}

	payments, err := q.AuditOneOffPaymentsWithSpec(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("query one-off payments: %w", err)
	}

	for _, payment := range payments {
		if len(payment.EntitlementsSpec) == 0 {
			continue
		}
		var spec map[string]*int
		if err := json.Unmarshal(payment.EntitlementsSpec, &spec); err != nil {
			return nil, fmt.Errorf("unmarshal entitlements_spec for product %s: %w", payment.ProductID, err)
		}
		if spec == nil {
			continue
		}

		// Check if any entitlement was granted from this payment
		count, err := q.AuditOneOffPaymentEntitlementCount(ctx, payment.ID)
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
				UserID:         payment.TenantSubjectID.String(),
				Description:    fmt.Sprintf("Completed one-off payment %s for product '%s' has no entitlements granted", payment.ID, payment.ProductSlug),
				Recommendation: fmt.Sprintf("Grant entitlements from product.entitlements_spec with source_type=one_off, source_id=%s", payment.ID),
				AutoFixable:    true,
				Details: map[string]any{
					"product_slug": payment.ProductSlug,
					"product_id":   payment.ProductID,
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

func (c *CheckOrphanOneOffEntitlements) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find entitlements where source_type=one_off but payment doesn't exist or has amount <= 0 (refund)
	results, err := q.AuditOrphanOneOffEntitlements(ctx)
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
			EntityID:       r.EntID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Entitlement '%s' references payment %s which is invalid (%s)", r.Entitlement, r.SourceID, reason),
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

func (c *CheckRefundedPaymentActiveEntitlements) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Find payments where total refunds >= original amount but entitlements still active
	results, err := q.AuditRefundedPaymentActiveEntitlements(ctx)
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
			EntityID:       r.EntID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Payment %s was fully refunded (%s of %s) but entitlement '%s' is still active", r.PaymentID, moneyutil.FormatUSD(r.RefundedAmount), moneyutil.FormatUSD(r.OriginalAmount), r.Entitlement),
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
