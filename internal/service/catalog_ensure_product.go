package service

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
	"strings"
)

// EnsureProduct creates the full declaration only if absent. Reuse never
// updates labels, tier entitlements or lifecycle; those are explicit updates.
func (s *Service) EnsureProduct(ctx context.Context, req CreateProductRequest) (*CatalogProduct, error) {
	owned, err := catalogOwnerRequest(ctx)
	if err != nil {
		return nil, err
	}
	if owned && (req.EntitlementsSpec != nil || req.TierGroup != nil || req.TierRank != 0) {
		return nil, catalog.ErrOwnerOperation
	}
	if owned && !req.CatalogID.IsZero() && req.CatalogID.UUID() != *catalogscope.QueryID(ctx) {
		return nil, catalog.ErrOwnerScope
	}
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*CatalogProduct, error) {
		return scoped.ensureProduct(ctx, req, owned)
	})
}

func (s *Service) ensureProduct(ctx context.Context, req CreateProductRequest, owned bool) (*CatalogProduct, error) {
	req.Key = strings.TrimSpace(req.Key)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.Key == "" || req.DisplayName == "" {
		return nil, apperr.Invalidf("key and display_name required")
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var product *CatalogProduct
	err = s.catalogDatabase().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		scoped := *s
		scoped.catalogTx = s.catalogDatabase().NewWithPgxTx(tx)
		if err := lockCatalogKey(ctx, tx, mid, "product", req.Key); err != nil {
			return err
		}
		if req.CatalogID.IsZero() {
			if owned {
				req.CatalogID = openrails.CatalogID(*catalogscope.QueryID(ctx))
			} else {
				row, err := catalog.NewCatalogRepo(scoped.catalogTx).Ensure(ctx, nil)
				if err != nil {
					return err
				}
				req.CatalogID = openrails.CatalogID(row.ID)
			}
		}
		var err error
		product, err = scoped.GetProductByKey(ctx, req.Key)
		if errors.Is(err, openrails.ErrNotFound) {
			err = scoped.catalogTx.MerchantTx(ctx, func(ctx context.Context, createTx pgx.Tx) error {
				creating := scoped
				creating.catalogTx = scoped.catalogTx.NewWithPgxTx(createTx)
				var err error
				product, err = creating.CreateProduct(ctx, req)
				return err
			})
			if errors.Is(err, openrails.ErrConflict) {
				product, err = scoped.GetProductByKey(ctx, req.Key)
				if errors.Is(err, openrails.ErrNotFound) {
					return ErrCatalogConflict
				}
			}
		}
		if err != nil {
			return err
		}
		if product.CatalogID != req.CatalogID {
			return ErrCatalogConflict
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return product, nil
}
