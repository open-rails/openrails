package audit

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Severity levels for audit findings
type Severity string

const (
	SeverityCritical Severity = "CRITICAL"
	SeverityHigh     Severity = "HIGH"
	SeverityMedium   Severity = "MEDIUM"
	SeverityLow      Severity = "LOW"
)

// EntityType identifies the type of entity affected
type EntityType string

const (
	EntitySubscription     EntityType = "subscription"
	EntityEntitlement      EntityType = "entitlement"
	EntityPayment          EntityType = "payment"
	EntityPaymentMethod    EntityType = "payment_method"
	EntityEntitlementGrant EntityType = "admin_grant"
	EntityProduct          EntityType = "product"
	EntityPrice            EntityType = "price"
)

// Finding represents a single consistency issue found during audit
type Finding struct {
	CheckID        string     `json:"check_id"`
	CheckName      string     `json:"check_name"`
	Severity       Severity   `json:"severity"`
	EntityType     EntityType `json:"entity_type"`
	EntityID       uuid.UUID  `json:"entity_id"`
	UserID         string     `json:"user_id"`
	Description    string     `json:"description"`
	Recommendation string     `json:"recommendation"`
	AutoFixable    bool       `json:"auto_fixable"`
	Details        any        `json:"details,omitempty"`
}

// Summary provides aggregate statistics for the audit
type Summary struct {
	TotalFindings    int              `json:"total_findings"`
	BySeverity       map[Severity]int `json:"by_severity"`
	ByCategory       map[string]int   `json:"by_category"`
	AutoFixable      int              `json:"auto_fixable"`
	ManualReviewOnly int              `json:"manual_review_only"`
	Duration         time.Duration    `json:"duration"`
}

// Options configures the audit run
type Options struct {
	// Filter by severity (empty means all)
	Severity Severity
	// Filter to specific user
	UserID string
	// Only check records created after this date
	Since *time.Time
	// Output format
	Format string
	// Attempt auto-fixes
	Fix bool
	// Specific categories to run (empty means all)
	Categories []string
}

// Check is the interface that all audit checks must implement
type Check interface {
	// ID returns the unique identifier for this check (e.g., "S-E-1")
	ID() string
	// Name returns the human-readable name
	Name() string
	// Category returns the check category (e.g., "subscription_entitlement")
	Category() string
	// Severity returns the severity level of issues found by this check
	Severity() Severity
	// Run executes the check and returns any findings
	Run(ctx context.Context, q *gen.Queries, opts Options) ([]Finding, error)
}

// Checker runs all registered checks
type Checker struct {
	q      *gen.Queries
	checks []Check
}

// NewChecker creates a new audit checker with all checks registered.
// qx is any sqlc queryable (*pgxpool.Pool, pgx.Tx, ...).
func NewChecker(qx gen.DBTX) *Checker {
	c := &Checker{q: gen.New(qx)}
	c.registerAllChecks()
	return c
}

// registerAllChecks registers all available audit checks
// NOTE: Checks for states that are now impossible due to database constraints
// have been removed. See migration 007_add_consistency_constraints.up.sql
func (c *Checker) registerAllChecks() {
	c.checks = []Check{
		// =====================================================================
		// Subscription-Entitlement checks (cross-table business logic)
		// These cannot be prevented by constraints as they involve relationships
		// between subscriptions and entitlements tables
		// =====================================================================
		&CheckActiveSubscriptionMissingEntitlements{},   // S-E-1
		&CheckOrphanSubscriptionEntitlements{},          // S-E-2
		&CheckCancelledSubscriptionActiveEntitlements{}, // S-E-3
		&CheckWrongEntitlementEndDate{},                 // S-E-4
		&CheckEntitlementSourceMismatch{},               // S-E-5

		// =====================================================================
		// Payment-Entitlement checks (cross-table business logic)
		// =====================================================================
		&CheckCompletedPaymentMissingEntitlements{}, // P-E-1
		&CheckOrphanOneOffEntitlements{},            // P-E-2
		&CheckRefundedPaymentActiveEntitlements{},   // P-E-3

		// =====================================================================
		// Duplicate checks
		// D-1, ES-5: Prevented by partial unique indexes
		// D-3: Only partially prevented (same start_at), true overlaps need btree_gist
		// =====================================================================
		&CheckDuplicateChargesSamePeriod{}, // D-2: Complex temporal grouping, cannot be constrained

		// =====================================================================
		// Subscription state checks
		// SS-2: Prevented by chk_cancelled_has_timestamp and chk_cancelled_has_type
		// SS-4: Prevented by chk_valid_period
		// SS-5: Prevented by chk_ended_not_before_cancelled
		// =====================================================================
		&CheckActiveSubscriptionPastPeriodEnd{}, // SS-1: Time-based, needs worker to fix
		&CheckPastDueWithoutRetry{},             // SS-3: Business logic for dunning

		// =====================================================================
		// Entitlement state checks
		// ES-1, ES-2: Prevented by chk_revoke_fields_together
		// ES-3: Prevented by chk_valid_time_window
		// ES-5: Prevented by uniq_entitlements_active partial unique index
		// =====================================================================
		// (All entitlement state checks now have constraints)

		// =====================================================================
		// Payment method checks (cross-table business logic)
		// PM-3: Prevented by FK constraint fk_subscriptions_payment_method_id
		// =====================================================================
		&CheckActiveSubscriptionFailedPaymentMethod{}, // PM-1
		&CheckExpiredCardActiveSubscription{},         // PM-2
		&CheckProcessorMismatch{},                     // PM-4

		// =====================================================================
		// Foreign key checks
		// FK-1, FK-2: Prevented by fk_subscriptions_price_id and fk_subscriptions_product
		// Subscription price/product coherence is prevented by subscriptions_price_product_merchant_fkey.
		// FK-4: Prevented by FK constraint on payments.subscription_id
		// =====================================================================
		&CheckEntitlementOrphanSource{}, // FK-5: Polymorphic FK, cannot be DB constraint

		// =====================================================================
		// Admin grant checks (cross-table business logic)
		// =====================================================================
		// AG-3 (revoked admin grant with active entitlement) was removed during
		// the sqlc migration: openrails.admin_grants has no revoked_at column, so
		// the bun-era check could never have executed.
		&CheckEntitlementGrantMissingEntitlements{},      // AG-1
		&CheckOrphanAdminEntitlements{},                  // AG-2
		&CheckExpiredEntitlementGrantActiveEntitlement{}, // AG-4

		// =====================================================================
		// Temporal checks
		// T-3: Prevented by chk_payment_not_future
		// =====================================================================
		&CheckStalePendingSubscription{},      // T-1: Business logic
		&CheckStalePastDueMaxRetries{},        // T-2: Business logic
		&CheckEntitlementDistantFutureStart{}, // T-4: Warning only, not enforced
	}
}

// Run executes all checks matching the given options
func (c *Checker) Run(ctx context.Context, opts Options) ([]Finding, Summary, error) {
	start := time.Now()
	var allFindings []Finding
	summary := Summary{
		BySeverity: make(map[Severity]int),
		ByCategory: make(map[string]int),
	}

	for _, check := range c.checks {
		// Filter by category if specified
		if len(opts.Categories) > 0 && !slices.Contains(opts.Categories, check.Category()) {
			continue
		}

		// Filter by severity if specified
		if opts.Severity != "" && check.Severity() != opts.Severity {
			continue
		}

		findings, err := check.Run(ctx, c.q, opts)
		if err != nil {
			return nil, summary, fmt.Errorf("check %s failed: %w", check.ID(), err)
		}

		allFindings = append(allFindings, findings...)

		for _, f := range findings {
			summary.BySeverity[f.Severity]++
			summary.ByCategory[check.Category()]++
			if f.AutoFixable {
				summary.AutoFixable++
			} else {
				summary.ManualReviewOnly++
			}
		}
	}

	summary.TotalFindings = len(allFindings)
	summary.Duration = time.Since(start)

	return allFindings, summary, nil
}

// GetCategories returns all unique check categories
func (c *Checker) GetCategories() []string {
	seen := make(map[string]bool)
	var categories []string
	for _, check := range c.checks {
		cat := check.Category()
		if !seen[cat] {
			seen[cat] = true
			categories = append(categories, cat)
		}
	}
	return categories
}
