package catalog

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// CatalogRepo persists business ownership within the selected merchant. It
// accepts owner subjects from trusted host authority, never authenticates them.
type CatalogRepo struct{ db *db.DB }

func NewCatalogRepo(database *db.DB) *CatalogRepo { return &CatalogRepo{db: database} }

func catalogMerchant(ctx context.Context) (merchant.ID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return merchant.ID{}, err
	}
	if scope, ok := catalogscope.FromContext(ctx); ok && scope.MerchantID != mid {
		return merchant.ID{}, fmt.Errorf("catalog scope does not match the authorized merchant")
	}
	return mid, nil
}

// Ensure preserves opaque subject bytes; nil selects the merchant default.
func (r *CatalogRepo) Ensure(ctx context.Context, ownerSubject *string) (gen.OpenrailsCatalog, error) {
	mid, err := catalogMerchant(ctx)
	if err != nil {
		return gen.OpenrailsCatalog{}, err
	}
	var row gen.OpenrailsCatalog
	err = r.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := gen.New(tx).LockCatalogRevision(ctx, mid.UUID()); err != nil {
			return err
		}
		scoped := NewCatalogRepo(r.db.NewWithPgxTx(tx))
		var err error
		row, err = scoped.ensure(ctx, ownerSubject)
		return err
	})
	return row, err
}

func (r *CatalogRepo) ensure(ctx context.Context, ownerSubject *string) (gen.OpenrailsCatalog, error) {
	mid, err := catalogMerchant(ctx)
	if err != nil {
		return gen.OpenrailsCatalog{}, err
	}
	if ownerSubject != nil {
		if err := catalogscope.ValidateSubject(*ownerSubject); err != nil {
			return gen.OpenrailsCatalog{}, err
		}
	}
	if scope, ok := catalogscope.FromContext(ctx); ok {
		if ownerSubject == nil || *ownerSubject != scope.OwnerSubject {
			return gen.OpenrailsCatalog{}, fmt.Errorf("catalog owner does not match the verified subject")
		}
		row, err := r.Get(ctx, scope.CatalogID)
		if err != nil {
			return row, err
		}
		if row.OwnerSubject == nil || *row.OwnerSubject != scope.OwnerSubject {
			return gen.OpenrailsCatalog{}, pgx.ErrNoRows
		}
		return row, nil
	}
	if ownerSubject == nil {
		return r.db.Gen(ctx).EnsureDefaultCatalog(ctx, mid.UUID())
	}
	return r.db.Gen(ctx).EnsureOwnedCatalog(ctx, gen.EnsureOwnedCatalogParams{MerchantID: mid.UUID(), OwnerSubject: *ownerSubject})
}

func (r *CatalogRepo) Get(ctx context.Context, id uuid.UUID) (gen.OpenrailsCatalog, error) {
	mid, err := catalogMerchant(ctx)
	if err != nil {
		return gen.OpenrailsCatalog{}, err
	}
	return r.db.Gen(ctx).GetCatalog(ctx, gen.GetCatalogParams{MerchantID: mid.UUID(), ID: id, CatalogID: catalogscope.QueryID(ctx)})
}

func (r *CatalogRepo) List(ctx context.Context, limit, offset int32) ([]gen.OpenrailsCatalog, error) {
	mid, err := catalogMerchant(ctx)
	if err != nil {
		return nil, err
	}
	if limit < 0 || offset < 0 {
		return nil, fmt.Errorf("catalog pagination must be nonnegative")
	}
	if limit == 0 {
		limit = 100
	}
	return r.db.Gen(ctx).ListCatalogs(ctx, gen.ListCatalogsParams{MerchantID: mid.UUID(), CatalogID: catalogscope.QueryID(ctx), PageLimit: limit, PageOffset: offset})
}

// GetByOwner is an exact, side-effect-free lookup in the authorized merchant.
func (r *CatalogRepo) GetByOwner(ctx context.Context, subject string) (gen.OpenrailsCatalog, error) {
	mid, err := catalogMerchant(ctx)
	if err != nil {
		return gen.OpenrailsCatalog{}, err
	}
	if err := catalogscope.ValidateSubject(subject); err != nil {
		return gen.OpenrailsCatalog{}, err
	}
	return r.db.Gen(ctx).GetCatalogByOwner(ctx, gen.GetCatalogByOwnerParams{
		MerchantID: mid.UUID(), OwnerSubject: subject, CatalogID: catalogscope.QueryID(ctx),
	})
}
