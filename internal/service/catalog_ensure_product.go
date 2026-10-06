package service

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// EnsureProduct creates the full declaration only if absent. Reuse never
// updates labels, tier entitlements or lifecycle; those are explicit updates.
func (s *Service) EnsureProduct(ctx context.Context, req billing.CreateProductParams) (*billing.Product, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Product, error) {
		return scoped.ensureProduct(ctx, req)
	})
}

func (s *Service) ensureProduct(ctx context.Context, req billing.CreateProductParams) (*billing.Product, error) {
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
	var product *billing.Product
	err = s.catalogDatabase().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		scoped := *s
		scoped.catalogTx = s.catalogDatabase().NewWithPgxTx(tx)
		if err := lockCatalogKey(ctx, tx, mid, "product", req.Key); err != nil {
			return err
		}
		var err error
		product, err = scoped.GetProductByKey(ctx, req.Key)
		if errors.Is(err, billing.ErrNotFound) {
			err = scoped.catalogTx.MerchantTx(ctx, func(ctx context.Context, createTx pgx.Tx) error {
				creating := scoped
				creating.catalogTx = scoped.catalogTx.NewWithPgxTx(createTx)
				var err error
				product, err = creating.CreateProduct(ctx, req)
				return err
			})
			if errors.Is(err, billing.ErrConflict) {
				product, err = scoped.GetProductByKey(ctx, req.Key)
				if errors.Is(err, billing.ErrNotFound) {
					return ErrCatalogConflict
				}
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return product, nil
}
