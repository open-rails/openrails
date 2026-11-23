package guide

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/open-rails/openrails"
)

// EmbeddedHealth serves readiness from the engine and logs the optional probes.
func EmbeddedHealth(ctx context.Context, mux *http.ServeMux, bill *openrails.Engine, logger *slog.Logger) {
	// guide:embedded-health
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if err := bill.Ready(r.Context()); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	for _, p := range bill.Probes() { // optional parts: report them, never fail on them
		if err := p.Check(ctx); err != nil {
			logger.WarnContext(ctx, "billing degraded", "probe", p.Name, "error", err)
		}
	}
	// guide:end
}
