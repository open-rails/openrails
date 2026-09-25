package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
)

// The standalone server exports every dependency's state, optional ones
// included, so an operator can alert on a degraded Redis, Vault or PSP.
func TestStandaloneDependencyMetrics(t *testing.T) {
	rec := httptest.NewRecorder()
	writeDependencyMetrics(rec, []app.ReadinessDependency{
		{Name: "postgres", Available: true},
		{Name: "redis", Optional: true, Err: errors.New("down")},
		{Name: "vault", Optional: true, Available: true},
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
	body := rec.Body.String()
	require.Contains(t, body, "# TYPE openrails_dependency_up gauge")
	require.Contains(t, body, `openrails_dependency_up{dependency="postgres",class="required"} 1`)
	require.Contains(t, body, `openrails_dependency_up{dependency="redis",class="optional"} 0`)
	require.Contains(t, body, `openrails_dependency_up{dependency="vault",class="optional"} 1`)

	rec = httptest.NewRecorder()
	(&Server{}).metricsHandler(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Contains(t, rec.Body.String(), `openrails_dependency_up{dependency="runtime",class="required"} 0`)
}
