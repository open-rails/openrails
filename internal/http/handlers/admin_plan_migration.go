package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// #813 plan-migration HTTP surface: POST /v1/merchant/plan-migrations
// (commit), GET .../preview (the operator's commit gate), GET /:id (batch +
// per-subscription ledger), POST /:id/cancel. Mounted next to the #773
// reprice routes — same authz, same error vocabulary.

type planMigrationRequestBody struct {
	SourcePrice string `json:"source_price"`
	TargetPrice string `json:"target_price"`
	// EffectiveAt: first renewal on/after this instant. Mutually exclusive
	// with NoticeDays; both empty means "now" (= each sub's next renewal).
	EffectiveAt time.Time `json:"effective_at,omitzero"`
	// NoticeDays computes effective_at = now + notice_days.
	NoticeDays             int    `json:"notice_days,omitempty"`
	Immediate              bool   `json:"immediate,omitempty"`
	AcknowledgeShortNotice bool   `json:"acknowledge_short_notice,omitempty"`
	FallbackPolicy         string `json:"fallback_policy,omitempty"`
	ArchiveSource          *bool  `json:"archive_source,omitempty"`
}

func (b *planMigrationRequestBody) toServiceRequest(r *httprequest.Request) (subscriptions.PlanMigrationRequest, bool) {
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
		effective = time.Now().UTC().Add(time.Duration(b.NoticeDays) * 24 * time.Hour)
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
	var body planMigrationRequestBody
	if !r.BindJSON(&body) {
		return
	}
	req, ok := body.toServiceRequest(r)
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
	var body planMigrationRequestBody
	if !r.BindJSON(&body) {
		return
	}
	req, ok := body.toServiceRequest(r)
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

// GetPlanMigration returns one migration batch header + its per-subscription
// ledger rows.
func GetPlanMigration(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid plan migration id")
		return
	}
	if r.State.PlanMigrationService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "plan migration service unavailable")
		return
	}
	limit := parseIntDefault(r.Query("limit"), 500)
	offset := parseIntDefault(r.Query("offset"), 0)
	batch, rows, err := r.State.PlanMigrationService.GetBatch(r.Request.Context(), id, limit, offset)
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.JSON(http.StatusOK, map[string]any{"batch": batch, "subscriptions": rows})
}

// CancelPlanMigration cancels every still-scheduled row in the batch.
func CancelPlanMigration(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid plan migration id")
		return
	}
	if r.State.PlanMigrationService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "plan migration service unavailable")
		return
	}
	res, err := r.State.PlanMigrationService.CancelBatch(r.Request.Context(), id)
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.JSON(http.StatusOK, res)
}
