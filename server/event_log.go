package server

import (
	"context"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/eventlog"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ListEvents is the privileged platform's read-only log for one merchant,
// including a soft-deleted or archived merchant. As with GetMerchant, the
// caller authorizes and audits access. It rechecks directory existence on
// every call; cursors remain bound to this merchant and filter. Ordinary
// Client/HTTP calls still require a live merchant.
func (s *Server) ListEvents(ctx context.Context, id billing.MerchantID, query billing.EventQuery) (*billing.EventPage, error) {
	if scoped, ok := merchant.FromContext(ctx); ok && scoped != id {
		return nil, apperr.Invalidf("event merchant differs from the authorized scope")
	}
	if _, err := s.GetMerchant(ctx, id); err != nil {
		return nil, err
	}
	rt := s.graph.Runtime
	retained, err := config.EventRetentionDuration(rt.Config)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if rt.Clock != nil {
		now = rt.Clock.Now()
	}
	var page billing.EventPage
	err = rt.DB.RunInMerchantScope(ctx, id, "retained event log", func(ctx context.Context) error {
		var err error
		page, err = eventlog.List(ctx, rt.DB, id, query, retained, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &page, nil
}
