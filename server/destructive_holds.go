package server

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/pagination"
)

// ListOpenDestructiveHolds pages the deployment's open incidents, oldest
// first, with their private evidence, for an operator the caller has
// authenticated. Clearing one is the merchant handle's ClearDestructiveHold,
// made ActingFor that operator.
func (s *Server) ListOpenDestructiveHolds(ctx context.Context, page billing.PageRequest) (*billing.ListPage[billing.DestructiveHoldReview], error) {
	limit, err := pagination.Limit(page)
	if err != nil {
		return nil, err
	}
	var cursor struct {
		ID uuid.UUID `json:"i"`
	}
	var after *uuid.UUID
	if present, err := pagination.Decode(page.Cursor, &cursor); err != nil {
		return nil, err
	} else if present {
		after = &cursor.ID
	}
	gate := s.graph.Runtime.DestructiveAdmissions
	addresses, err := gate.ListOpenHoldsDirectory(ctx, limit+1, after)
	if err != nil {
		return nil, err
	}
	reviews := make([]billing.DestructiveHoldReview, 0, len(addresses))
	for _, address := range addresses {
		hold, err := gate.GetHold(ctx, address.MerchantID, address.ID)
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, hold.Review())
	}
	out := pagination.Cut(reviews, limit, func(r billing.DestructiveHoldReview) any {
		return struct {
			ID uuid.UUID `json:"i"`
		}{r.Hold.ID.UUID()}
	})
	return &out, nil
}
