package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/app"
)

// metricsHandler serves /metrics: one gauge per dependency Ready reports,
// optional ones included, from the same cached state (no provider calls).
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var runtime *app.Runtime
	if s != nil {
		runtime = s.runtime
	}
	deps, _ := runtime.Ready(ctx)
	writeDependencyMetrics(w, deps)
}

func writeDependencyMetrics(w http.ResponseWriter, deps []app.ReadinessDependency) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "# HELP openrails_dependency_up Whether a dependency is usable (1) or not (0).")
	_, _ = fmt.Fprintln(w, "# TYPE openrails_dependency_up gauge")
	for _, d := range deps {
		class, up := "required", 0
		if d.Optional {
			class = "optional"
		}
		if d.Available {
			up = 1
		}
		_, _ = fmt.Fprintf(w, "openrails_dependency_up{dependency=%q,class=%q} %d\n", d.Name, class, up)
	}
}
