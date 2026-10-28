package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/app"
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
// the abuse state memory took while Redis did not answer, and each
// background job kind's health.
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var runtime *app.Runtime
	var abuse *abusestate.Store
	if s != nil {
		runtime = s.runtime
		abuse = s.abuseState()
	}
	deps, _ := runtime.Ready(ctx)
	var workers []billing.WorkerHealth
	if runtime != nil && runtime.DB != nil {
		var err error
		if workers, err = runtime.ListWorkerHealth(ctx); err != nil {
			log.WithError(err).Warn("metrics: job health")
		}
	}
	writeMetrics(w, deps, abuse, workers)
}

func writeMetrics(w http.ResponseWriter, deps []app.ReadinessDependency, abuse *abusestate.Store, workers []billing.WorkerHealth) {
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
	if abuse.UsesRedis() {
		_, _ = fmt.Fprintln(w, "# HELP openrails_abuse_state_fallbacks_total Rate-limit, lockout and captcha operations kept in this process's memory because Redis did not answer.")
		_, _ = fmt.Fprintln(w, "# TYPE openrails_abuse_state_fallbacks_total counter")
		_, _ = fmt.Fprintf(w, "openrails_abuse_state_fallbacks_total %d\n", abuse.Fallbacks())
	}
	if len(workers) == 0 {
		return
	}
	for _, gauge := range []struct {
		name, help string
		value      func(billing.WorkerHealth) int64
	}{
		{"openrails_job_overrunning", "Jobs of a kind running past their timeout.", func(h billing.WorkerHealth) int64 { return h.Overrunning }},
		{"openrails_job_retryable", "Jobs of a kind waiting to retry after an error.", func(h billing.WorkerHealth) int64 { return h.Retryable }},
		{"openrails_job_discarded", "Jobs of a kind that ran out of attempts (River keeps them a week).", func(h billing.WorkerHealth) int64 { return h.Discarded }},
	} {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", gauge.name, gauge.help, gauge.name)
		for _, h := range workers {
			_, _ = fmt.Fprintf(w, "%s{kind=%q} %d\n", gauge.name, h.Kind, gauge.value(h))
		}
	}
	_, _ = fmt.Fprintln(w, "# HELP openrails_job_last_completed_timestamp_seconds When a periodic job kind last completed (Unix seconds; absent: never).")
	_, _ = fmt.Fprintln(w, "# TYPE openrails_job_last_completed_timestamp_seconds gauge")
	for _, h := range workers {
		if h.LastCompletedAt != nil {
			_, _ = fmt.Fprintf(w, "openrails_job_last_completed_timestamp_seconds{kind=%q} %d\n", h.Kind, h.LastCompletedAt.Unix())
		}
	}
}
