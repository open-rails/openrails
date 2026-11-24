package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/billing"
)

// CreatePriceMigration records the migration, then archives its sources as
// any catalog edit does: under the catalog lock, admitted, and owned by the
// edit so a catalog document does not quietly sell them again. A replay
// archives what the first call did not.
func (s *Service) CreatePriceMigration(ctx context.Context, p billing.CreatePriceMigrationParams) (*billing.PriceMigration, error) {
	if s.rt.PriceMigrationService == nil {
		return nil, errors.New("price migration service unavailable")
	}
	migration, archive, err := s.rt.PriceMigrationService.Create(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, id := range archive {
		if _, err := s.DeactivatePrice(ctx, id); err != nil {
			return nil, fmt.Errorf("price migration %s recorded; archiving %s: %w", migration.ID, id, err)
		}
	}
	return migration, nil
}
