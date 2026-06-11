package handlers

import (
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

const defaultAdminOperationsLimit = 50

func adminOperationsPagination(r *httprequest.Request) (int, int) {
	limit, _ := strconv.Atoi(r.Request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = defaultAdminOperationsLimit
	}
	offset, _ := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func GetAdminRepairAlerts(r *httprequest.Request) {
	ctx := r.Request.Context()
	limit, offset := adminOperationsPagination(r)
	tsid, err := repo.ResolveTenantSubjectID(ctx, r.State.DB.Qx(ctx), uuid.Nil, "system")
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to resolve tenant subject")
		return
	}

	var seen *bool
	seenParam := strings.ToLower(strings.TrimSpace(r.Request.URL.Query().Get("seen")))
	if seenParam == "true" || seenParam == "false" {
		v := seenParam == "true"
		seen = &v
	}

	q := r.State.DB.Gen(ctx)
	total, err := q.CountRepairAlerts(ctx, gen.CountRepairAlertsParams{
		TenantSubjectID: tsid, EventType: string(models.NotificationSystemAlert), Seen: seen,
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to count repair alerts")
		return
	}
	rows, err := q.ListRepairAlerts(ctx, gen.ListRepairAlertsParams{
		TenantSubjectID: tsid, EventType: string(models.NotificationSystemAlert), Seen: seen,
		Column3: int32(min(limit, math.MaxInt32)), Column4: int32(min(offset, math.MaxInt32)),
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve repair alerts")
		return
	}
	items := make([]*models.NotificationQueue, 0, len(rows))
	for _, row := range rows {
		m, merr := repo.NotificationFromGen(row)
		if merr != nil {
			r.ErrorJSON(http.StatusInternalServerError, "failed to decode repair alerts")
			return
		}
		items = append(items, m)
	}
	r.SuccessJSONPaginated(items, total, limit, offset)
}

func GetAdminManualRebillAttempts(r *httprequest.Request) {
	ctx := r.Request.Context()
	limit, offset := adminOperationsPagination(r)

	var statusFilter *string
	status := strings.ToLower(strings.TrimSpace(r.Request.URL.Query().Get("status")))
	if status == "" {
		status = string(models.ManualRebillAttemptUnknown)
	}
	if status != "all" {
		switch models.ManualRebillAttemptStatus(status) {
		case models.ManualRebillAttemptPending, models.ManualRebillAttemptSucceeded, models.ManualRebillAttemptFailed, models.ManualRebillAttemptUnknown:
			statusFilter = &status
		default:
			r.ErrorJSON(http.StatusBadRequest, "invalid status")
			return
		}
	}

	var processorFilter *string
	if processor := strings.TrimSpace(r.Request.URL.Query().Get("processor")); processor != "" {
		processorFilter = &processor
	}

	q := r.State.DB.Gen(ctx)
	total, err := q.CountManualRebillAttempts(ctx, gen.CountManualRebillAttemptsParams{
		Status: statusFilter, Processor: processorFilter,
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to count manual rebill attempts")
		return
	}
	rows, err := q.ListManualRebillAttempts(ctx, gen.ListManualRebillAttemptsParams{
		Status: statusFilter, Processor: processorFilter,
		Column1: int32(min(limit, math.MaxInt32)), Column2: int32(min(offset, math.MaxInt32)),
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve manual rebill attempts")
		return
	}
	items := make([]*models.ManualRebillAttempt, 0, len(rows))
	for _, row := range rows {
		items = append(items, &models.ManualRebillAttempt{
			ID:             row.ID,
			SubscriptionID: row.SubscriptionID,
			PeriodEnd:      row.PeriodEnd,
			Processor:      models.Processor(row.Processor),
			OrderReference: row.OrderReference,
			Status:         models.ManualRebillAttemptStatus(row.Status),
			TransactionID:  row.TransactionID,
			FailureReason:  row.FailureReason,
			ClaimedUntil:   row.ClaimedUntil,
			CreatedAt:      row.CreatedAt,
			UpdatedAt:      row.UpdatedAt,
		})
	}
	r.SuccessJSONPaginated(items, total, limit, offset)
}
