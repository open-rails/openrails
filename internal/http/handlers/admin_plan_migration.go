package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// Plan migrations: POST /v1/merchant/plan-migrations commits one and
// .../preview is the operator's commit gate. The migration is a reprice
// batch of kind plan_change, read and canceled at /reprice-batches/{id}.

func planMigrationServiceRequest(r *httprequest.Request, b billing.PlanMigrationRequest) (subscriptions.PlanMigrationRequest, bool) {
	var out subscriptions.PlanMigrationRequest
	if strings.TrimSpace(b.SourcePrice) == "" || strings.TrimSpace(b.TargetPrice) == "" {
		r.ErrorJSON(http.StatusBadRequest, "source_price and target_price required")
		return out, false
	}
	if !b.EffectiveAt.IsZero() && b.NoticeDays > 0 {
		r.ErrorJSON(http.StatusBadRequest, "effective_at and notice_days are mutually exclusive")
		return out, false
	}
	ctx := r.Request.Context()
	if r.State.PlanMigrationService == nil || r.State.PriceService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "plan migration service unavailable")
		return out, false
	}
	source, err := catalog.ResolveReference(ctx, r.State.PriceService, b.SourcePrice)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "source_price not found")
		return out, false
	}
	target, err := catalog.ResolveReference(ctx, r.State.PriceService, b.TargetPrice)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "target_price not found")
		return out, false
	}
	effective := b.EffectiveAt
	if b.NoticeDays > 0 {
		effective = r.Clock.Now().UTC().Add(time.Duration(b.NoticeDays) * 24 * time.Hour)
	}
	out = subscriptions.PlanMigrationRequest{
		SourcePriceID:          source.ID,
		TargetPriceID:          target.ID,
		EffectiveAt:            effective,
		Immediate:              b.Immediate,
		AcknowledgeShortNotice: b.AcknowledgeShortNotice,
		FallbackPolicy:         b.FallbackPolicy,
		ArchiveSource:          b.ArchiveSource,
	}
	return out, true
}

func writePlanMigrationError(r *httprequest.Request, err error) {
	switch {
	case errors.Is(err, subscriptions.ErrPlanMigrationSameProduct),
		errors.Is(err, subscriptions.ErrPlanMigrationBadFallback),
		errors.Is(err, subscriptions.ErrRepriceCrossCurrency),
		errors.Is(err, subscriptions.ErrRepriceInactivePrice):
		r.ErrorJSON(http.StatusBadRequest, err.Error())
	default:
		writeRepriceError(r, err)
	}
}

// CreatePlanMigration commits a plan migration: batch + per-subscription
// rows, source archive, rail pushes, schedule-time notices.
func CreatePlanMigration(r *httprequest.Request) {
	var body billing.PlanMigrationRequest
	if !r.BindJSON(&body) {
		return
	}
	req, ok := planMigrationServiceRequest(r, body)
	if !ok {
		return
	}
	out, err := r.State.PlanMigrationService.Migrate(r.Request.Context(), req)
	if err != nil {
		writePlanMigrationError(r, err)
		return
	}
	r.JSON(http.StatusCreated, out)
}

// PreviewPlanMigration classifies the cohort without writing anything — the
// per-rail auto/requires-action/skip counts the operator reviews BEFORE
// committing.
func PreviewPlanMigration(r *httprequest.Request) {
	var body billing.PlanMigrationRequest
	if !r.BindJSON(&body) {
		return
	}
	req, ok := planMigrationServiceRequest(r, body)
	if !ok {
		return
	}
	out, err := r.State.PlanMigrationService.Preview(r.Request.Context(), req)
	if err != nil {
		writePlanMigrationError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}
