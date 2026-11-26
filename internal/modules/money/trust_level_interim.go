package money

import (
	"context"

	"github.com/google/uuid"

	identity "github.com/open-rails/openrails/internal/billingidentity"
)

// GetTrustLevel and TrustLevels keep admission on the effective tier
// until it reads EffectiveTrustLevel itself.
func (s *MoneyService) GetTrustLevel(ctx context.Context, payer identity.CustomerID, currency string) (string, error) {
	level, err := s.TrustLevel(ctx, payer, currency)
	return level.TrustLevel, err
}

func (s *MoneyService) TrustLevels(ctx context.Context, customers []uuid.UUID, currency string) (map[uuid.UUID]string, error) {
	levels, err := s.TrustLevelsIn(ctx, customers, currency)
	out := make(map[uuid.UUID]string, len(levels))
	for id, l := range levels {
		out[id] = l.TrustLevel
	}
	return out, err
}
