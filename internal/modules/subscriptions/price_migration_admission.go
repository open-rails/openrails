package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
)

var errMigrationUncertain = errors.New("price migration command is incomplete; reconcile its admitted targets before another migration")

func (s *PriceMigrationService) finishMigrationAdmission(ctx context.Context, a destructive.Action, outcome destructive.Outcome, result string) error {
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return s.admissions.Finish(finish, a.MerchantID, a.ID, outcome, result)
}

func (s *PriceMigrationService) admitMigration(ctx context.Context, request billing.CreatePriceMigrationParams, plan *migrationPlan, classes []*classified) (*destructive.Action, *billing.PriceMigration, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, nil, err
	}
	actor, actorErr := destructive.ActorFromContext(ctx)
	target := "price-migration/" + plan.target.PublicID.String()
	digest := destructive.Digest(request)
	if s.admissions != nil && actorErr == nil {
		action, found, err := s.admissions.FindPending(ctx, mid.UUID(), target, actor, digest)
		if err != nil {
			return nil, nil, err
		}
		if found {
			if !strings.HasPrefix(action.Result, "migration:") || !strings.HasSuffix(action.Result, ":prepared") {
				return nil, nil, errMigrationUncertain
			}
			raw := strings.TrimSuffix(strings.TrimPrefix(action.Result, "migration:"), ":prepared")
			id, err := uuid.Parse(raw)
			if err != nil {
				return nil, nil, errMigrationUncertain
			}
			out, err := s.Get(ctx, billing.PriceMigrationID(id))
			return nil, out, err
		}
	}
	targets := []string{}
	for _, c := range classes {
		if c.outcome.Disposition == billing.MigrationSkipped {
			continue
		}
		targets = append(targets, "subscription:"+c.sub.ID.String()+":"+c.sub.PriceID.String())
	}
	if plan.archive {
		for id, source := range plan.sources {
			if !source.Archived {
				targets = append(targets, "price:"+id.String())
			}
		}
	}
	if len(targets) == 0 {
		return nil, nil, nil
	}
	if s.admissions == nil {
		return nil, nil, errors.New("price migration destructive admission is not wired")
	}
	if actorErr != nil {
		return nil, nil, actorErr
	}
	sort.Strings(targets)
	action, err := s.admissions.Admit(ctx, destructive.Command{MerchantID: mid.UUID(), Kind: destructive.CatalogChange, Target: target,
		Key: "price-migration:" + destructive.Digest([]any{digest, targets}), Digest: digest, Units: int64(len(targets)), Actor: actor})
	if err != nil {
		return nil, nil, err
	}
	if action.Replayed {
		// Never create a second migration for an admission whose insert response
		// may have been lost. An incomplete original stays held for reconciliation.
		row, err := s.db.Gen(ctx).GetPriceMigrationByDestructiveAction(ctx, gen.GetPriceMigrationByDestructiveActionParams{MerchantID: mid.UUID(), DestructiveActionID: action.ID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
		if err == nil && row.ID != uuid.Nil {
			return nil, nil, errMigrationUncertain
		}
	}
	if _, err = s.admissions.Start(ctx, mid.UUID(), action.ID); err != nil {
		return nil, nil, err
	}
	return &action, nil, nil
}

func (s *PriceMigrationService) finishSettledMigration(ctx context.Context, id billing.PriceMigrationID) error {
	if s.admissions == nil {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	row, err := s.db.Gen(ctx).GetPriceMigration(ctx, gen.GetPriceMigrationParams{MerchantID: mid.UUID(), ID: id.UUID()})
	if err != nil {
		return err
	}
	actionID := row.BillingPriceMigration.DestructiveActionID
	if actionID == nil {
		return nil
	}
	action, err := s.admissions.GetAction(ctx, mid.UUID(), *actionID)
	if err != nil {
		return err
	}
	if action.State == destructive.StateSucceeded || action.State == destructive.StateNoEffect {
		return nil
	}
	if action.Result != fmt.Sprintf("migration:%s:prepared", id.UUID()) {
		return nil
	}
	if row.Scheduled > 0 || row.Blocked > 0 {
		return nil
	}
	return s.finishMigrationAdmission(ctx, action, destructive.OutcomeSucceeded, id.String())
}

// finishSettledMigrations releases completed cohorts through the existing
// re-driver. The lookup materializes IDs before any admission transaction, so
// no domain lock or open rows are held while the receipt is finalized.
func (s *PriceMigrationService) finishSettledMigrations(ctx context.Context, limit int) error {
	if s.admissions == nil {
		return nil
	}
	bound := int32(min(max(limit, 1), 1000))
	merchants, err := s.db.GenDirectory().ListSettledPriceMigrationMerchants(ctx, bound)
	if err != nil {
		return err
	}
	remaining := bound
	for _, id := range merchants {
		if remaining == 0 {
			break
		}
		err = s.db.RunInMerchantScope(ctx, billing.MerchantID(id), "settle price migration admission", func(ctx context.Context) error {
			actions, err := s.db.Gen(ctx).ListSettledPriceMigrationActions(ctx, gen.ListSettledPriceMigrationActionsParams{MerchantID: id, RowLimit: remaining})
			if err != nil {
				return err
			}
			for _, actionID := range actions {
				if err := s.admissions.Finish(ctx, id, actionID, destructive.OutcomeSucceeded, ""); err != nil {
					return err
				}
				remaining--
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
