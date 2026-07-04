//go:build integration

package metrics_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/metrics"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// End-to-end denial pipeline: hot-path Redis counter -> periodic flush worker
// -> admission_denials_hourly -> metrics query.
func TestMetrics_DenialFlushEndToEnd(t *testing.T) {
	_, svc, _, ctxB := seed(t)
	rdb, rctx := dbtest.SharedRedisClient(t)
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))

	now := time.Now().UTC()
	rec := admission.NewDenialRecorder(rdb)
	rec.Record(rctx, mB.String(), cB.String(), "insufficient_credit", now)
	rec.Record(rctx, mB.String(), cB.String(), "insufficient_credit", now)
	rec.Record(rctx, mB.String(), cB.String(), "budget_exceeded", now)

	w := riverjobs.AdmissionDenialFlushWorker{DB: dbi, Redis: rdb}
	require.NoError(t, w.Work(context.Background(), nil))

	from := now.AddDate(0, 0, -1).Format("2006-01-02")
	to := now.AddDate(0, 0, 1).Format("2006-01-02")
	res := run(t, svc, ctxB, &metrics.Query{
		Measures: []string{"admission_denials"},
		By:       []string{"denial_reason"},
		Range:    &metrics.QueryRange{From: from, To: to},
	})
	require.Equal(t, int64(2), cell(t, res, map[string]string{"denial_reason": "insufficient_credit"}, "admission_denials"))
	require.Equal(t, int64(1), cell(t, res, map[string]string{"denial_reason": "budget_exceeded"}, "admission_denials"))

	// Counters were drained (flush subtracts exactly what it wrote).
	key := admission.DenialKey(mB.String(), now)
	remaining, err := rdb.HGet(rctx, key, cB.String()+"|insufficient_credit").Result()
	if err == nil {
		require.Equal(t, "0", remaining)
	}

	// A second flush must not double-count.
	require.NoError(t, w.Work(context.Background(), nil))
	res2 := run(t, svc, ctxB, &metrics.Query{
		Measures: []string{"admission_denials"},
		Range:    &metrics.QueryRange{From: from, To: to},
	})
	require.Equal(t, int64(3), cell(t, res2, map[string]string{}, "admission_denials"))
}
