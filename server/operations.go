package server

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// Operator operations with no HTTP route: the openrails CLI calls them, and a
// hosted product may build its own operator pages on them.

// ListWorkerHealth is every background job kind's recent runs, with the
// verbatim error text, which can name any merchant's records. The private
// listener's /metrics carries the same as gauges.
func (s *Server) ListWorkerHealth(ctx context.Context) ([]billing.WorkerHealth, error) {
	return riverjobs.ListWorkerHealth(ctx, s.graph.Runtime.DB)
}

// UnlockAdminLockout ends a person's lockout from administrative operations
// (the per-administrator operation limits) and resets their counters. Without
// Redis a lockout lives in the process that imposed it.
func (s *Server) UnlockAdminLockout(ctx context.Context, userID string) error {
	return s.surface.UnlockAdminLockout(ctx, userID, "operator")
}

// PrivateHandler is the operator's private surface (GET /metrics), which Run
// and Serve listen on at Config.PrivateAddr. Never mount it on a public
// listener.
func (s *Server) PrivateHandler() http.Handler { return s.surface.PrivateHandler() }
