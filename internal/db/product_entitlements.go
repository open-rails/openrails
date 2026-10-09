package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// ProductFromGen maps a product row with its current entitlement keys.
func (d *DB) ProductFromGen(ctx context.Context, row gen.BillingProduct) (*models.Product, error) {
	product, err := models.ProductFromGen(row)
	if err != nil {
		return nil, err
	}
	if err := d.LoadProductEntitlements(ctx, product); err != nil {
		return nil, err
	}
	return product, nil
}

// ProductsFromGen maps product rows with their current keys in one query.
func (d *DB) ProductsFromGen(ctx context.Context, rows []gen.BillingProduct) ([]*models.Product, error) {
	out := make([]*models.Product, 0, len(rows))
	for _, row := range rows {
		product, err := models.ProductFromGen(row)
		if err != nil {
			return nil, err
		}
		out = append(out, product)
	}
	return out, d.LoadProductEntitlements(ctx, out...)
}

// LoadProductEntitlements sets each product's current keys, in byte order.
func (d *DB) LoadProductEntitlements(ctx context.Context, products ...*models.Product) error {
	if len(products) == 0 {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(products))
	byID := make(map[uuid.UUID][]*models.Product, len(products))
	for _, product := range products {
		if product == nil {
			continue
		}
		if product.MerchantID != mid.UUID() {
			return fmt.Errorf("product %s does not belong to scoped merchant", product.ID)
		}
		product.Entitlements = []string{}
		if _, seen := byID[product.ID]; !seen {
			ids = append(ids, product.ID)
		}
		byID[product.ID] = append(byID[product.ID], product)
	}
	rows, err := d.Gen(ctx).ListLiveProductEntitlements(ctx, gen.ListLiveProductEntitlementsParams{MerchantID: mid.UUID(), ProductIds: ids})
	if err != nil {
		return err
	}
	for _, row := range rows {
		for _, product := range byID[row.ProductID] {
			product.Entitlements = append(product.Entitlements, row.Entitlement)
		}
	}
	return nil
}

// LiveProductEntitlementsJSON is one product's current keys as a sorted JSON
// list, the shape accepted snapshots record.
func LiveProductEntitlementsJSON(ctx context.Context, q *gen.Queries, merchantID, productID uuid.UUID) ([]byte, error) {
	rows, err := q.ListLiveProductEntitlements(ctx, gen.ListLiveProductEntitlementsParams{MerchantID: merchantID, ProductIds: []uuid.UUID{productID}})
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.Entitlement)
	}
	return json.Marshal(keys)
}
