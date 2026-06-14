package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
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
	tsid := repo.SystemCustomerID

	var seen *bool
	seenParam := strings.ToLower(strings.TrimSpace(r.Request.URL.Query().Get("seen")))
	if seenParam == "true" || seenParam == "false" {
		v := seenParam == "true"
		seen = &v
	}

	q := r.State.DB.Gen(ctx)
	total, err := q.CountRepairAlerts(ctx, gen.CountRepairAlertsParams{
		CustomerID: tsid, EventType: string(models.NotificationSystemAlert), Seen: seen,
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to count repair alerts")
		return
	}
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := q.ListRepairAlerts(ctx, gen.ListRepairAlertsParams{
		CustomerID: tsid, EventType: string(models.NotificationSystemAlert), Seen: seen,
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
// (from the retired openrails.manual_rebill_attempts table) onto provider
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

// executesUnderMode reports the weakest operating mode that lets an intent of
// this origin attempt its provider write (the GateExecution matrix):
// user/admin-origin executes under limited, system-origin requires full,
// nothing executes under readonly.
func executesUnderMode(origin string) string {
	if intents.Origin(origin) == intents.OriginSystem {
		return "full"
	}
	return "limited"
}

// GetAdminProviderIntents lists the provider intent ledger (#358) — every
// outbound provider mutation OpenRails wants to perform, with status and
// type/provider/subscription filters. Under mode=limited/readonly this is the
// dry-run view: the pending rows are exactly what the executor will drain
// when the mode lifts, and executes_under says which mode each needs.
func GetAdminProviderIntents(r *httprequest.Request) {
	ctx := r.Request.Context()
	limit, offset := adminOperationsPagination(r)

	var statusFilter *string
	status := strings.ToLower(strings.TrimSpace(r.Request.URL.Query().Get("status")))
	if status == "" {
		status = "pending"
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
	var typeFilter *string
	if t := strings.ToLower(strings.TrimSpace(r.Request.URL.Query().Get("type"))); t != "" {
		typeFilter = &t
	}
	var subscriptionFilter *uuid.UUID
	if s := strings.TrimSpace(r.Request.URL.Query().Get("subscription_id")); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid subscription_id")
			return
		}
		subscriptionFilter = &id
	}

	q := r.State.DB.Gen(ctx)
	total, err := q.CountProviderIntents(ctx, gen.CountProviderIntentsParams{
		Status: statusFilter, Provider: processorFilter, IntentType: typeFilter, SubscriptionID: subscriptionFilter,
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to count provider intents")
		return
	}
	rows, err := q.ListProviderIntents(ctx, gen.ListProviderIntentsParams{
		Status: statusFilter, Provider: processorFilter, IntentType: typeFilter, SubscriptionID: subscriptionFilter,
		PageLimit: int64(limit), PageOffset: int64(offset),
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve provider intents")
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := map[string]any{
			"id":                  row.ID,
			"type":                row.IntentType,
			"origin":              row.Origin,
			"executes_under":      executesUnderMode(row.Origin),
			"processor":           row.Provider,
			"subscription_id":     row.SubscriptionID,
			"payment_id":          row.PaymentID,
			"status":              row.Status,
			"attempts":            row.Attempts,
			"next_attempt_at":     row.NextAttemptAt,
			"failure_reason":      row.LastFailureReason,
			"origin_reason":       row.OriginReason,
			"expires_at":          row.ExpiresAt,
			"executed_at":         row.ExecutedAt,
			"created_at":          row.CreatedAt,
			"updated_at":          row.UpdatedAt,
			"account_fingerprint": row.AccountFingerprint,
		}
		if len(row.Payload) > 0 {
			item["payload"] = json.RawMessage(row.Payload)
		}
		if len(row.ResultEvidence) > 0 {
			item["result_evidence"] = json.RawMessage(row.ResultEvidence)
		}
		items = append(items, item)
	}
	r.SuccessJSONPaginated(items, total, limit, offset)
}
