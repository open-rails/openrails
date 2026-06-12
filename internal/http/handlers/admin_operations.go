package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/intents"
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
	tsid := repo.SystemTenantSubjectID

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
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := q.ListRepairAlerts(ctx, gen.ListRepairAlertsParams{
		TenantSubjectID: tsid, EventType: string(models.NotificationSystemAlert), Seen: seen,
		Column3: limit32, Column4: offset32,
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

// manualRebillStatusFilters maps the endpoint's legacy status vocabulary
// (from the retired billing.manual_rebill_attempts table) onto provider
// intent ledger statuses (#358 phase C).
var manualRebillStatusFilters = map[string]string{
	"pending":                        intents.StatusPending,
	"succeeded":                      intents.StatusSucceeded,
	"failed":                         intents.StatusFailedTerminal,
	"unknown":                        intents.StatusUnknownNeedsVerify,
	intents.StatusInFlight:           intents.StatusInFlight,
	intents.StatusFailedRetryable:    intents.StatusFailedRetryable,
	intents.StatusFailedTerminal:     intents.StatusFailedTerminal,
	intents.StatusUnknownNeedsVerify: intents.StatusUnknownNeedsVerify,
	intents.StatusSuperseded:         intents.StatusSuperseded,
	intents.StatusExpired:            intents.StatusExpired,
}

// GetAdminManualRebillAttempts lists manual_rebill provider intents — the
// dunning charges folded onto the intent ledger. Defaults to the rows needing
// operator attention (unknown_needs_verify), like the retired claim-table
// view did.
func GetAdminManualRebillAttempts(r *httprequest.Request) {
	ctx := r.Request.Context()
	limit, offset := adminOperationsPagination(r)

	var statusFilter *string
	status := strings.ToLower(strings.TrimSpace(r.Request.URL.Query().Get("status")))
	if status == "" {
		status = "unknown"
	}
	if status != "all" {
		mapped, ok := manualRebillStatusFilters[status]
		if !ok {
			r.ErrorJSON(http.StatusBadRequest, "invalid status")
			return
		}
		statusFilter = &mapped
	}

	var processorFilter *string
	if processor := strings.ToLower(strings.TrimSpace(r.Request.URL.Query().Get("processor"))); processor != "" {
		processorFilter = &processor
	}
	intentType := intents.TypeManualRebill

	q := r.State.DB.Gen(ctx)
	total, err := q.CountProviderIntents(ctx, gen.CountProviderIntentsParams{
		Status: statusFilter, Provider: processorFilter, IntentType: &intentType,
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to count manual rebill intents")
		return
	}
	rows, err := q.ListProviderIntents(ctx, gen.ListProviderIntentsParams{
		Status: statusFilter, Provider: processorFilter, IntentType: &intentType,
		PageLimit: int64(limit), PageOffset: int64(offset),
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve manual rebill intents")
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := map[string]any{
			"id":              row.ID,
			"subscription_id": row.SubscriptionID,
			"processor":       row.Provider,
			"status":          row.Status,
			"attempts":        row.Attempts,
			"next_attempt_at": row.NextAttemptAt,
			"failure_reason":  row.LastFailureReason,
			"expires_at":      row.ExpiresAt,
			"executed_at":     row.ExecutedAt,
			"created_at":      row.CreatedAt,
			"updated_at":      row.UpdatedAt,
		}
		var payload intents.ManualRebillPayload
		if len(row.Payload) > 0 && json.Unmarshal(row.Payload, &payload) == nil {
			item["period_end"] = payload.PeriodEnd
			item["order_reference"] = payload.OrderReference
			item["attempt"] = payload.Attempt
		}
		var evidence map[string]any
		if len(row.ResultEvidence) > 0 && json.Unmarshal(row.ResultEvidence, &evidence) == nil {
			if txn, ok := evidence["transaction_id"].(string); ok {
				item["transaction_id"] = txn
			}
		}
		items = append(items, item)
	}
	r.SuccessJSONPaginated(items, total, limit, offset)
}
