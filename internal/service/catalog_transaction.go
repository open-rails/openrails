package service

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// catalogMutation serializes catalog changes per merchant. Nested writes share
// one local transaction and cannot commit early.
func catalogMutation[T any](ctx context.Context, s *Service, fn func(context.Context, *Service) (T, error)) (out T, err error) {
	if s == nil || s.rt == nil {
		return out, fmt.Errorf("catalog service not initialized")
	}
	if err = catalogpolicy.Check(ctx, s.rt.CatalogEdits); err != nil {
		return out, err
	}
	if s.catalogWriteLocked {
		return fn(ctx, s)
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return out, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	var committedWork []func(context.Context, *Service)
	err = s.catalogDatabase().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCatalogRevision(ctx, mid.UUID()); err != nil {
			return err
		}
		before, err := q.ListRecurringBenefitOverlaps(ctx, mid.UUID())
		if err != nil {
			return err
		}
		scoped := *s
		scoped.catalogTx = s.catalogDatabase().NewWithPgxTx(tx)
		scoped.catalogWriteLocked = true
		scoped.catalogCommittedWork = &committedWork
		var callErr error
		if out, callErr = fn(ctx, &scoped); callErr != nil {
			return callErr
		}
		return refuseNewBenefitOverlap(ctx, q, mid.UUID(), before)
	})
	if err == nil {
		for _, work := range committedWork {
			work(ctx, s)
		}
	}
	return out, err
}

// catalogAfterCommit keeps best-effort legacy propagation outside the local
// transaction. Failed transactions discard all callbacks; none are retried.
func (s *Service) catalogAfterCommit(ctx context.Context, work func(context.Context, *Service)) {
	if s.localCatalogOnly {
		return
	}
	if s.catalogWriteLocked {
		if s.catalogCommittedWork != nil {
			*s.catalogCommittedWork = append(*s.catalogCommittedWork, work)
		}
		return
	}
	work(ctx, s)
}

// checkCatalogWritePolicy refuses a write before any provider work it
// would start; catalogMutation checks again under the lock.
func (s *Service) checkCatalogWritePolicy(ctx context.Context) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("catalog service not initialized")
	}
	if err := catalogpolicy.Check(ctx, s.rt.CatalogEdits); err != nil {
		return err
	}
	return nil
}

// CatalogRevision is a read-only merchant revision lookup. A paginated caller
// must pin/check this value across its reads and restart if it changes.
func (s *Service) CatalogRevision(ctx context.Context) (int64, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	return s.catalogDatabase().Gen(ctx).GetCatalogRevision(ctx, mid.UUID())
}

// ErrCatalogBenefitOverlap refuses a catalog change that lets two recurring
// products grant one entitlement outside a shared tier group.
func errCatalogBenefitOverlap(o gen.ListRecurringBenefitOverlapsRow) error {
	return apperr.New(http.StatusConflict, billing.CodeCatalogBenefitOverlap, fmt.Sprintf("products %q and %q both grant %q on a recurring price; put them in one tier group so a customer cannot pay for it twice", o.FirstProduct, o.SecondProduct, o.Entitlement))
}

// refuseNewBenefitOverlap refuses an overlap the mutation created. An overlap
// the catalog already held does not block unrelated edits.
func refuseNewBenefitOverlap(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, before []gen.ListRecurringBenefitOverlapsRow) error {
	after, err := q.ListRecurringBenefitOverlaps(ctx, merchantID)
	if err != nil {
		return err
	}
	known := make(map[gen.ListRecurringBenefitOverlapsRow]bool, len(before))
	for _, o := range before {
		known[o] = true
	}
	for _, o := range after {
		if !known[o] {
			return errCatalogBenefitOverlap(o)
		}
	}
	return nil
}
