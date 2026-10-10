package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// PrivateHandler is the operator's private surface, never the public one:
// GET /metrics.
func (s *Server) PrivateHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.metricsHandler)
	return mux
}

// UnlockAdminLockout ends userID's administrative-operation lockout and
// resets its counters; actor is recorded as who unlocked it.
func (s *Server) UnlockAdminLockout(ctx context.Context, userID, actor string) error {
	return s.adminLimiter.Unlock(ctx, userID, actor)
}

// metricsHandler serves /metrics: one gauge per dependency Ready reports,
// optional ones included, from the same cached state (no provider calls),
// and each background job kind's health.
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var runtime *app.Runtime
	if s != nil {
		runtime = s.runtime
	}
	deps, _ := runtime.Ready(ctx)
	var workers []billing.WorkerHealth
	if runtime != nil && runtime.DB != nil {
		var err error
		if workers, err = riverjobs.ListWorkerHealth(ctx, runtime.DB); err != nil {
			log.WithError(err).Warn("metrics: worker health")
		}
	}
	writeMetrics(w, deps, workers)
}

func writeMetrics(w http.ResponseWriter, deps []app.ReadinessDependency, workers []billing.WorkerHealth) {
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
	if len(workers) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "# HELP openrails_worker_consecutive_failures Failed runs of a job kind since its last success.")
	_, _ = fmt.Fprintln(w, "# TYPE openrails_worker_consecutive_failures gauge")
	for _, h := range workers {
		_, _ = fmt.Fprintf(w, "openrails_worker_consecutive_failures{kind=%q} %d\n", h.WorkerKind, h.ConsecutiveFailures)
	}
	_, _ = fmt.Fprintln(w, "# HELP openrails_worker_last_success_timestamp_seconds When a job kind last succeeded (Unix seconds; absent: never).")
	_, _ = fmt.Fprintln(w, "# TYPE openrails_worker_last_success_timestamp_seconds gauge")
	for _, h := range workers {
		if h.LastSuccessAt != nil {
			_, _ = fmt.Fprintf(w, "openrails_worker_last_success_timestamp_seconds{kind=%q} %d\n", h.WorkerKind, h.LastSuccessAt.Unix())
		}
	}
}
