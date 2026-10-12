package server

import (
	"context"

	"github.com/open-rails/openrails/billing"
)

// ListOpenDestructiveHolds is the trusted operator's deployment-wide incident list.
func (s *Server) ListOpenDestructiveHolds(ctx context.Context, limit int, after billing.DestructiveHoldID) ([]billing.DestructiveHoldReview, error) {
	return s.engine.ListOpenDestructiveHolds(ctx, limit, after)
}

// ClearDestructiveHold records the verified human operator and reason. A hosted
// caller must authorize that operator before invoking this in-process method.
func (s *Server) ClearDestructiveHold(ctx context.Context, merchant billing.MerchantID, id billing.DestructiveHoldID, operator billing.DestructiveOperator, reason string) (*billing.DestructiveHold, error) {
	return s.engine.ClearDestructiveHold(ctx, merchant, id, operator, reason)
}
