package service

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

func (s *Service) catalogDatabase() *db.DB {
	if s.catalogTx != nil {
		return s.catalogTx
	}
	return s.rt.DB
}

// createPriceWithProduct owns the local product/price transaction. A natural
// product key reuses an existing product without changing labels;
// each explicitly keyed immutable price can be retried concurrently.
func (s *Service) createPriceWithProduct(ctx context.Context, req billing.CreatePriceParams) (*billing.Price, error) {
	if !req.ProductID.IsZero() || req.ProductKey != "" {
		return nil, apperr.Invalidf("product_id, product_key and product_data are mutually exclusive")
	}
	data := *req.ProductData
	data.Key = strings.TrimSpace(data.Key)
	data.DisplayName = strings.TrimSpace(data.DisplayName)
	if data.Key == "" || data.DisplayName == "" {
		return nil, apperr.Invalidf("product_data requires key and display_name")
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
	var out *billing.Price
	err = s.catalogDatabase().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		scoped := *s
		scoped.catalogTx = s.catalogDatabase().NewWithPgxTx(tx)
		scoped.localCatalogOnly = true
		product, err := scoped.EnsureProduct(ctx, billing.CreateProductParams{Key: data.Key, DisplayName: data.DisplayName, Description: data.Description})
		if err != nil {
			return err
		}
		req.ProductID = product.ID
		req.ProductData = nil
		req.Currency = money.NormalizeCurrency(req.Currency)
		products, prices, err := scoped.requireCatalogServices()
		if err != nil {
			return err
		}
		model, err := products.GetByID(ctx, product.ID.UUID())
		if err != nil {
			return err
		}
		key, defaulted := resolvePriceKey(model, req)
		if err := lockCatalogKey(ctx, tx, mid, "price-key", key); err != nil {
			return err
		}
		current, err := prices.GetCurrentByKey(ctx, mid.UUID(), model.ID, key)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := defaultKeyCadenceConflict(defaulted, current, req, key); err != nil {
			return err
		}
		terms := req
		terms.Key = key
		if current != nil && !samePriceTerms(current.View(), terms) {
			return ErrCatalogConflict
		}
		existing, err := prices.FindByTerms(ctx, req, key)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if existing != nil && existing.Archived != req.Archived {
			return ErrCatalogConflict
		}
		out, err = scoped.CreatePrice(ctx, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
