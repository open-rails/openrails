package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

func (s *Service) catalogDatabase() *db.DB {
	if s.catalogTx != nil {
		return s.catalogTx
	}
	return s.rt.DB
}

// createPriceWithProduct owns the local product/price transaction. A natural
// product key reuses an existing same-catalog product without changing labels;
// each explicitly keyed immutable price can be retried concurrently.
func (s *Service) createPriceWithProduct(ctx context.Context, req CreatePriceRequest) (*CatalogPrice, error) {
	if !req.ProductID.IsZero() || req.ProductKey != "" {
		return nil, apperr.Invalidf("product_id, product_key and product_data are mutually exclusive")
	}
	data := *req.ProductData
	data.Key = strings.TrimSpace(data.Key)
	data.DisplayName = strings.TrimSpace(data.DisplayName)
	if data.Key == "" || data.DisplayName == "" {
		return nil, apperr.Invalidf("product_data requires key and display_name")
	}
	requested := openrails.CatalogID{}
	if data.CatalogID != "" {
		var err error
		requested, err = openrails.ParseCatalogID(data.CatalogID)
		if err != nil || requested.IsZero() {
			return nil, apperr.Invalidf("invalid product_data.catalog_id")
		}
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
	var out *CatalogPrice
	err = s.catalogDatabase().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		scoped := *s
		scoped.catalogTx = s.catalogDatabase().NewWithPgxTx(tx)
		scoped.localCatalogOnly = true
		product, err := scoped.EnsureProduct(ctx, CreateProductRequest{CatalogID: requested, Key: data.Key, DisplayName: data.DisplayName, Description: data.Description})
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
		key := resolvePriceKey(model, req)
		if err := lockCatalogKey(ctx, tx, mid, "price-key", key); err != nil {
			return err
		}
		current, err := prices.GetCurrentByKey(ctx, mid.UUID(), key)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		expected := priceDeterministicID(product.ID.UUID(), req.UnitAmount, req.Currency, req.AccessDurationHours, req.AutoRenew, req.TrialUnitAmount, req.TrialDurationHours)
		if current != nil && (current.ProductID != product.ID.UUID() || current.ID != expected) {
			return ErrCatalogConflict
		}
		// Same financial substance under a second key must not relabel an existing
		// offer: callers retain immutable references for their own revision checks.
		existing, err := prices.GetByID(ctx, expected)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if existing != nil && existing.ID != uuid.Nil && (existing.Key != key || existing.Archived != req.Archived) {
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
