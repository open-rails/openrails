package audit

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/db/gen"
)

// FK-1: Orphan subscription product
type CheckOrphanSubscriptionProduct struct{}

func (c *CheckOrphanSubscriptionProduct) ID() string         { return "FK-1" }
func (c *CheckOrphanSubscriptionProduct) Name() string       { return "orphan_subscription_product" }
func (c *CheckOrphanSubscriptionProduct) Category() string   { return "foreign_key" }
func (c *CheckOrphanSubscriptionProduct) Severity() Severity { return SeverityHigh }

func (c *CheckOrphanSubscriptionProduct) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditOrphanSubscriptionProduct(ctx)
	if err != nil {
		return nil, fmt.Errorf("query orphan subscription products: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		issue := "does not exist"
		if r.ProdExists && r.ProdActive != nil && !*r.ProdActive {
			issue = "is inactive"
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.SubID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Subscription references product %s which %s", r.ProductID, issue),
			Recommendation: "MANUAL REVIEW - Product may have been deleted or deactivated",
			AutoFixable:    false,
			Details: map[string]any{
				"product_id":     r.ProductID,
				"product_exists": r.ProdExists,
				"product_active": r.ProdActive,
			},
		})
	}

	return findings, nil
}

// FK-2: Orphan subscription price
type CheckOrphanSubscriptionPrice struct{}

func (c *CheckOrphanSubscriptionPrice) ID() string         { return "FK-2" }
func (c *CheckOrphanSubscriptionPrice) Name() string       { return "orphan_subscription_price" }
func (c *CheckOrphanSubscriptionPrice) Category() string   { return "foreign_key" }
func (c *CheckOrphanSubscriptionPrice) Severity() Severity { return SeverityHigh }

func (c *CheckOrphanSubscriptionPrice) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditOrphanSubscriptionPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("query orphan subscription prices: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		issue := "does not exist"
		if r.PriceExists && r.PriceActive != nil && !*r.PriceActive {
			issue = "is inactive"
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntitySubscription,
			EntityID:       r.SubID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Subscription references price %s which %s", r.PriceID, issue),
			Recommendation: "MANUAL REVIEW - Price may have been deleted or deactivated",
			AutoFixable:    false,
			Details: map[string]any{
				"price_id":     r.PriceID,
				"price_exists": r.PriceExists,
				"price_active": r.PriceActive,
			},
		})
	}

	return findings, nil
}

// FK-4: Payment references non-existent subscription
type CheckPaymentOrphanSubscription struct{}

func (c *CheckPaymentOrphanSubscription) ID() string         { return "FK-4" }
func (c *CheckPaymentOrphanSubscription) Name() string       { return "payment_orphan_subscription" }
func (c *CheckPaymentOrphanSubscription) Category() string   { return "foreign_key" }
func (c *CheckPaymentOrphanSubscription) Severity() Severity { return SeverityMedium }

func (c *CheckPaymentOrphanSubscription) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	results, err := q.AuditPaymentOrphanSubscription(ctx)
	if err != nil {
		return nil, fmt.Errorf("query payment orphan subscription: %w", err)
	}

	for _, r := range results {
		if opts.UserID != "" && r.UserID != opts.UserID {
			continue
		}

		findings = append(findings, Finding{
			CheckID:        c.ID(),
			CheckName:      c.Name(),
			Severity:       c.Severity(),
			EntityType:     EntityPayment,
			EntityID:       r.PaymentID,
			UserID:         r.UserID,
			Description:    fmt.Sprintf("Payment references non-existent subscription %s", r.SubscriptionID),
			Recommendation: "Clear subscription_id (payment record is still valid)",
			AutoFixable:    true,
			Details: map[string]any{
				"missing_subscription_id": r.SubscriptionID,
			},
		})
	}

	return findings, nil
}

// FK-5: Entitlement references non-existent source
type CheckEntitlementOrphanSource struct{}

func (c *CheckEntitlementOrphanSource) ID() string         { return "FK-5" }
func (c *CheckEntitlementOrphanSource) Name() string       { return "entitlement_orphan_source" }
func (c *CheckEntitlementOrphanSource) Category() string   { return "foreign_key" }
func (c *CheckEntitlementOrphanSource) Severity() Severity { return SeverityMedium }

func (c *CheckEntitlementOrphanSource) Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error) {
	var findings []Finding

	// Check subscription sources
	subResults, err := q.AuditEntitlementOrphanSubscriptionSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("query entitlement orphan subscription: %w", err)
	}

	for _, r := range subResults {
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
			Description:    fmt.Sprintf("Entitlement '%s' references non-existent subscription %s", r.Entitlement, r.SourceID),
			Recommendation: "MANUAL REVIEW - May be valid historical entitlement",
			AutoFixable:    false,
			Details: map[string]any{
				"source_type":             r.SourceType,
				"missing_subscription_id": r.SourceID,
			},
		})
	}

	// Check one_off payment sources
	payResults, err := q.AuditEntitlementOrphanPaymentSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("query entitlement orphan payment: %w", err)
	}

	for _, r := range payResults {
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
			Description:    fmt.Sprintf("Entitlement '%s' references non-existent payment %s", r.Entitlement, r.SourceID),
			Recommendation: "MANUAL REVIEW - May be valid historical entitlement",
			AutoFixable:    false,
			Details: map[string]any{
				"source_type":        r.SourceType,
				"missing_payment_id": r.SourceID,
			},
		})
	}

	return findings, nil
}
