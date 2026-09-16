package openrails

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// PlanMigrationRequest moves a price's subscribers to a price of another
// product at each subscription's first renewal on or after EffectiveAt.
// Prices are addressed by ID or key.
type PlanMigrationRequest struct {
	SourcePrice string `json:"source_price"`
	TargetPrice string `json:"target_price"`
	// EffectiveAt and NoticeDays are mutually exclusive; both empty means now.
	EffectiveAt time.Time `json:"effective_at,omitzero"`
	NoticeDays  int       `json:"notice_days,omitempty"`
	// Immediate also applies access cutover now for auto-migratable
	// subscriptions; nothing is charged until the next invoice.
	Immediate bool `json:"immediate,omitempty"`
	// AcknowledgeShortNotice permits a price increase inside the merchant's
	// notice window.
	AcknowledgeShortNotice bool `json:"acknowledge_short_notice,omitempty"`
	// FallbackPolicy applies to rails that cannot be migrated server-side:
	// keep_grandfathered (default) or cancel_at_period_end.
	FallbackPolicy string `json:"fallback_policy,omitempty"`
	// ArchiveSource defaults to true on create; preview ignores it.
	ArchiveSource *bool `json:"archive_source,omitempty"`
}

// PlanMigrationOutcome classifies one subscription:
// scheduled, applied_immediately, skipped or blocked.
type PlanMigrationOutcome struct {
	SubscriptionID uuid.UUID  `json:"subscription_id"`
	RepriceID      *uuid.UUID `json:"reprice_id,omitempty"`
	Rail           string     `json:"rail"`
	Disposition    string     `json:"disposition"`
	Reason         string     `json:"reason,omitempty"`
}

// PlanMigrationRailCounts summarizes what each rail can migrate server-side.
type PlanMigrationRailCounts struct {
	Auto           int `json:"auto"`
	RequiresAction int `json:"requires_action"`
	Skipped        int `json:"skipped"`
}

// PlanMigrationResult is returned by preview (BatchID nil, nothing written)
// and create.
type PlanMigrationResult struct {
	BatchID        *uuid.UUID                          `json:"batch_id,omitempty"`
	SourcePriceID  uuid.UUID                           `json:"source_price_id"`
	TargetPriceID  uuid.UUID                           `json:"target_price_id"`
	EffectiveAt    time.Time                           `json:"effective_at"`
	FallbackPolicy string                              `json:"fallback_policy"`
	Matched        int                                 `json:"matched"`
	Scheduled      int                                 `json:"scheduled"`
	Skipped        int                                 `json:"skipped"`
	Blocked        int                                 `json:"blocked"`
	ByRail         map[string]*PlanMigrationRailCounts `json:"by_rail"`
	Outcomes       []PlanMigrationOutcome              `json:"outcomes"`
	SourceArchived bool                                `json:"source_archived"`
}

// PlanMigrationCancelResult reports scheduled rows canceled. Subscriptions in
// RailReleaseRequired still have a provider-side schedule to release.
type PlanMigrationCancelResult struct {
	Canceled            int         `json:"canceled"`
	RailReleaseRequired []uuid.UUID `json:"rail_release_required,omitempty"`
	Warning             string      `json:"warning,omitempty"`
}

// PreviewPlanMigration classifies the affected subscriptions without writing.
func (c *Client) PreviewPlanMigration(ctx context.Context, request PlanMigrationRequest) (*PlanMigrationResult, error) {
	var out PlanMigrationResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/plan-migrations/preview", request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreatePlanMigration schedules the migration and records its batch.
func (c *Client) CreatePlanMigration(ctx context.Context, request PlanMigrationRequest) (*PlanMigrationResult, error) {
	var out PlanMigrationResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/plan-migrations", request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelPlanMigration cancels the batch's still-scheduled subscriptions.
func (c *Client) CancelPlanMigration(ctx context.Context, batchID uuid.UUID) (*PlanMigrationCancelResult, error) {
	var out PlanMigrationCancelResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/plan-migrations/"+batchID.String()+"/cancel", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
