package productaccess

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

type KeyDecision struct {
	ProductID uuid.UUID
	HasAccess bool
}

// CheckProductKeys resolves one bounded key batch in the authorized merchant
// and catalog. Existing ownership survives catalog archival; unknown keys deny.
func (s *Service) CheckProductKeys(ctx context.Context, userID string, keys []string) (map[string]KeyDecision, error) {
	if len(keys) > 100 {
		return nil, errors.New("at most 100 products are allowed")
	}
	unique := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(key) == "" || !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
			return nil, errors.New("product_key is invalid")
		}
		if !seen[key] {
			seen[key] = true
			unique = append(unique, key)
		}
	}
	out := make(map[string]KeyDecision, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if owner, ok := catalogscope.FromContext(ctx); ok && (owner.MerchantID != mid || owner.CatalogID == uuid.Nil) {
		return nil, errors.New("invalid catalog scope")
	}
	customer, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	err = s.withTx(ctx, func(ctx context.Context, repo *ProductAccessGrantRepo) error {
		rows, err := repo.db.Gen(ctx).CheckProductAccessKeys(ctx, gen.CheckProductAccessKeysParams{MerchantID: mid.UUID(), CustomerID: customer, ProductKeys: unique, CatalogID: catalogscope.QueryID(ctx), AtTime: s.now().UTC()})
		if err != nil {
			return err
		}
		for _, row := range rows {
			decision := KeyDecision{HasAccess: row.HasAccess}
			if row.ProductID != nil {
				decision.ProductID = *row.ProductID
			}
			out[row.ProductKey] = decision
		}
		return nil
	})
	return out, err
}
