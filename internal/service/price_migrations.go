package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
)

// CreatePriceMigration admits the migration as one command, its subscribers
// and the sources it archives, archives the sources first, so no one buys
// them while the cohort is counted, then records it. An archive that fails
// is a warning on the migration, never a refusal of what happened.
func (s *Service) CreatePriceMigration(ctx context.Context, p billing.CreatePriceMigrationParams) (*billing.PriceMigration, error) {
	migrations := s.rt.PriceMigrationService
	if migrations == nil {
		return nil, errors.New("price migration service unavailable")
	}
	pending, prior, err := migrations.Admit(ctx, p)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		// A replay: what the first left on sale is archived as its own edit.
		for _, id := range pending.Archive {
			if _, err := s.DeactivatePrice(ctx, id); err != nil {
				prior.Warnings = append(prior.Warnings, archiveWarning(id, err))
			}
		}
		return prior, nil
	}
	var warnings []string
	archived := false
	for _, id := range pending.Archive {
		if err := s.archiveUnder(ctx, pending.Action, id); err != nil {
			warnings = append(warnings, archiveWarning(id, err))
			continue
		}
		archived = true
	}
	migration, err := migrations.Record(ctx, pending)
	if err != nil {
		return nil, migrations.Abandon(ctx, pending, archived, err)
	}
	migration.Warnings = append(migration.Warnings, warnings...)
	return migration, nil
}

func archiveWarning(id billing.PriceID, err error) string {
	return fmt.Sprintf("price %s is still on sale: archiving it failed (%v); archive it, then migrate its new subscribers", id, err)
}

// archiveUnder archives a source as any catalog edit does, under the catalog
// lock and owning the field, as part of the command action admitted: it
// spends no unit of its own and leaves the command to finish.
func (s *Service) archiveUnder(ctx context.Context, action *destructive.Action, id billing.PriceID) error {
	archived := true
	for attempt := 1; ; attempt++ {
		scope, err := catalogReadLocked(ctx, s, func(ctx context.Context, read *Service) (*catalogAdmission, error) {
			mid, err := merchant.Require(ctx)
			if err != nil {
				return nil, err
			}
			before, err := readCatalogState(ctx, read.catalogDatabase().Gen(ctx), mid.UUID())
			if err != nil {
				return nil, err
			}
			objects, err := priceEditPlan(id, UpdatePriceRequest{Archived: &archived})(ctx, read, before)
			if err != nil {
				return nil, err
			}
			targets := map[catalogObject]bool{}
			for _, obj := range objects {
				targets[obj] = true
			}
			return &catalogAdmission{before: before, targets: targets, action: action, outer: true}, nil
		})
		if err != nil {
			return err
		}
		if len(scope.targets) == 0 {
			return nil
		}
		_, err = s.DeactivatePrice(context.WithValue(ctx, catalogAdmissionKey{}, scope), id)
		if !errors.Is(err, errCatalogSnapshotMoved) || attempt == maxCatalogSnapshotAttempts {
			return err
		}
	}
}
