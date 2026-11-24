package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// CreatePriceMigration admits the migration as one command, its subscribers
// and the sources it archives, archives the sources first, so no one buys
// them while the cohort is counted, then records it. An archive that fails
// is a warning on the migration, never a refusal of what happened. Once a
// source is archived the migration is recorded even if the caller leaves;
// should recording fail, the command stays open and the identical retry
// records it under the same units.
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
		// A replay: what the first left on sale is archived under its command.
		for _, id := range pending.Archive {
			if _, err := s.archiveUnder(ctx, pending.Action, id); err != nil {
				prior.Warnings = append(prior.Warnings, archiveWarning(id, err))
			}
		}
		return prior, nil
	}
	if len(pending.Archive) > 0 {
		ctx = context.WithoutCancel(ctx)
	}
	var warnings []string
	archived := false
	for _, id := range pending.Archive {
		did, err := s.archiveUnder(ctx, pending.Action, id)
		if err != nil {
			warnings = append(warnings, archiveWarning(id, err))
		}
		archived = archived || did
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
// spends no unit of its own and leaves the command to finish. A catalog
// moved since its plan is planned again. It reports whether it archived.
func (s *Service) archiveUnder(ctx context.Context, action *destructive.Action, id billing.PriceID) (bool, error) {
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
			return false, err
		}
		if len(scope.targets) == 0 {
			return false, nil
		}
		// The mutation itself, not the command around it, which would retry
		// this stale plan.
		_, err = s.deactivatePriceCommand(context.WithValue(ctx, catalogAdmissionKey{}, scope), id)
		switch {
		case err == nil:
			return true, nil
		case !errors.Is(err, errCatalogSnapshotMoved):
			return false, err
		case attempt == maxCatalogSnapshotAttempts:
			return false, apperr.New(http.StatusConflict, billing.CodeCatalogRevisionConflict, "the catalog kept changing while archiving the source; retry")
		}
	}
}
