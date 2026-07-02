package handlers

import (
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// workerHealthItem is the admin view of one registered River worker kind (#689).
type workerHealthItem struct {
	WorkerKind            string     `json:"worker_kind"`
	RegisteredAt          time.Time  `json:"registered_at"`
	ExpectedPeriodSeconds *int64     `json:"expected_period_seconds,omitempty"`
	LastSuccessAt         *time.Time `json:"last_success_at,omitempty"`
	LastErrorAt           *time.Time `json:"last_error_at,omitempty"`
	LastError             *string    `json:"last_error,omitempty"`
	ConsecutiveFailures   int32      `json:"consecutive_failures"`
	LastAlertedAt         *time.Time `json:"last_alerted_at,omitempty"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

// GetAdminWorkerHealth lists every registered worker kind with its last
// success/error/streak (#689) — the "expected N runs, got 0" dashboard.
// Orchestration-free read: handler -> gen directly (worker_health is an
// operator-global control-plane table, no merchant scope).
func GetAdminWorkerHealth(r *httprequest.Request) {
	ctx := r.Request.Context()
	rows, err := r.State.DB.Gen(ctx).ListWorkerHealth(ctx)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve worker health")
		return
	}
	items := make([]workerHealthItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, workerHealthItemFromGen(row))
	}
	r.SuccessJSON(items)
}

func workerHealthItemFromGen(row gen.OpenrailsWorkerHealth) workerHealthItem {
	return workerHealthItem{
		WorkerKind:            row.WorkerKind,
		RegisteredAt:          row.RegisteredAt,
		ExpectedPeriodSeconds: row.ExpectedPeriodSeconds,
		LastSuccessAt:         row.LastSuccessAt,
		LastErrorAt:           row.LastErrorAt,
		LastError:             row.LastError,
		ConsecutiveFailures:   row.ConsecutiveFailures,
		LastAlertedAt:         row.LastAlertedAt,
		UpdatedAt:             row.UpdatedAt,
	}
}
