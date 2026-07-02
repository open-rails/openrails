//go:build integration

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

// #689: the admin worker-health endpoint returns every registered kind with its
// last success/error/streak — handler -> gen directly (operator-global table).
func TestGetAdminWorkerHealthReturnsRows(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	ctx := context.Background()

	kinds := []string{"test.admin_wh_ok", "test.admin_wh_failing"}
	t.Cleanup(func() {
		for _, kind := range kinds {
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.worker_health WHERE worker_kind = $1`, kind)
		}
	})
	_, err := dbi.Qx(ctx).Exec(ctx,
		`INSERT INTO openrails.worker_health (worker_kind, expected_period_seconds, last_success_at, consecutive_failures)
		 VALUES ($1, 3600, now(), 0)`, kinds[0])
	require.NoError(t, err)
	_, err = dbi.Qx(ctx).Exec(ctx,
		`INSERT INTO openrails.worker_health (worker_kind, last_error_at, last_error, consecutive_failures)
		 VALUES ($1, now(), 'boom', 7)`, kinds[1])
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	req := httprequest.NewHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/worker-health", nil),
		&app.Runtime{DB: dbi})
	GetAdminWorkerHealth(req)

	require.Equal(t, http.StatusOK, recorder.Code)
	var items []struct {
		WorkerKind            string     `json:"worker_kind"`
		ExpectedPeriodSeconds *int64     `json:"expected_period_seconds"`
		LastSuccessAt         *time.Time `json:"last_success_at"`
		LastError             *string    `json:"last_error"`
		ConsecutiveFailures   int32      `json:"consecutive_failures"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &items))

	byKind := map[string]int{}
	for i, item := range items {
		byKind[item.WorkerKind] = i
	}
	require.Contains(t, byKind, kinds[0])
	require.Contains(t, byKind, kinds[1])

	ok := items[byKind[kinds[0]]]
	require.NotNil(t, ok.LastSuccessAt)
	require.NotNil(t, ok.ExpectedPeriodSeconds)
	require.EqualValues(t, 3600, *ok.ExpectedPeriodSeconds)
	require.EqualValues(t, 0, ok.ConsecutiveFailures)

	failing := items[byKind[kinds[1]]]
	require.Nil(t, failing.LastSuccessAt)
	require.NotNil(t, failing.LastError)
	require.Equal(t, "boom", *failing.LastError)
	require.EqualValues(t, 7, failing.ConsecutiveFailures)
}
