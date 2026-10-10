package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
)

// The private listener exports every dependency's state, optional ones
// included, and each job kind's health, so an operator can alert on a
// degraded Redis, Vault or PSP and on a stalled worker.
func TestStandaloneMetrics(t *testing.T) {
	rec := httptest.NewRecorder()
	succeeded := time.Unix(1_700_000_000, 0)
	writeMetrics(rec, []app.ReadinessDependency{
		{Name: "postgres", Available: true},
		{Name: "redis", Optional: true, Err: errors.New("down")},
		{Name: "vault", Optional: true, Available: true},
	}, []billing.WorkerHealth{
		{WorkerKind: "invoice_sweep", LastSuccessAt: &succeeded},
		{WorkerKind: "renewals", ConsecutiveFailures: 3},
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
	body := rec.Body.String()
	require.Contains(t, body, "# TYPE openrails_dependency_up gauge")
	require.Contains(t, body, `openrails_dependency_up{dependency="postgres",class="required"} 1`)
	require.Contains(t, body, `openrails_dependency_up{dependency="redis",class="optional"} 0`)
	require.Contains(t, body, `openrails_dependency_up{dependency="vault",class="optional"} 1`)
	require.Contains(t, body, `openrails_worker_consecutive_failures{kind="renewals"} 3`)
	require.Contains(t, body, `openrails_worker_last_success_timestamp_seconds{kind="invoice_sweep"} 1700000000`)
	require.NotContains(t, body, `openrails_worker_last_success_timestamp_seconds{kind="renewals"}`)

	rec = httptest.NewRecorder()
	(&Server{}).PrivateHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Contains(t, rec.Body.String(), `openrails_dependency_up{dependency="runtime",class="required"} 0`)
}
