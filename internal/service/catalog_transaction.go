package service

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// catalogMutation fences every service authoring operation in merchant-first
// lock order. Nested writes share one local transaction and cannot commit early.
func catalogMutation[T any](ctx context.Context, s *Service, fn func(context.Context, *Service) (T, error)) (out T, err error) {
	if s == nil || s.rt == nil {
		return out, fmt.Errorf("catalog service not initialized")
	}
	if err = catalogpolicy.Check(ctx, s.rt.Config); err != nil {
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
		if _, err := gen.New(tx).LockCatalogRevision(ctx, mid.UUID()); err != nil {
			return err
		}
		scoped := *s
		scoped.catalogTx = s.catalogDatabase().NewWithPgxTx(tx)
		scoped.catalogWriteLocked = true
		scoped.catalogCommittedWork = &committedWork
		var callErr error
		out, callErr = fn(ctx, &scoped)
		return callErr
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

func (s *Service) checkCatalogWritePolicy(ctx context.Context) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("catalog service not initialized")
	}
	return catalogpolicy.Check(ctx, s.rt.Config)
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
