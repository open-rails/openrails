package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
)

// The standalone meta routes are mounted with no runtime of their own: they
// log through the server's logger.
func TestStandaloneMetaRoutesLogThroughTheServer(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	s := &Server{Log: logger, runtime: &app.Runtime{Log: logger, Config: &billing.Config{}}}
	mux := http.NewServeMux()
	s.registerStandaloneMetaRoutes(mux)
	for path, status := range map[string]int{"/": http.StatusOK, "/nope": http.StatusNotFound, "/health/live": http.StatusOK, "/health/nope": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != status {
			t.Fatalf("GET %s = %d, want %d: %s", path, rec.Code, status, rec.Body)
		}
	}
	s.draining.Store(true)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(out.String(), `"code":"service_unavailable"`) {
		t.Fatalf("draining /health/ready = %d, logged %q", rec.Code, out.String())
	}
}
