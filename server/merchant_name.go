package server

import (
	"context"
	"github.com/open-rails/openrails/billing"
)

// merchantName is a merchant group's name: its display name, else its slug.
func (s *Server) merchantName(ctx context.Context, groupID string) (string, error) {
	mid, err := billing.ParseMerchantID(groupID)
	if err != nil {
		return "", err
	}
	m, err := s.GetMerchant(ctx, mid)
	if err != nil {
		return "", err
	}
	if m.DisplayName != nil {
		return *m.DisplayName, nil
	}
	return m.Slug, nil
}
