package entitlements

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

// CheckMany resolves only the requested resource keys; it never enumerates a
// customer's complete entitlement history or consults mutable product contents.
func (s *EntitlementService) CheckMany(ctx context.Context, customerID string, keys []string, at time.Time) (map[string]bool, error) {
	if len(keys) > openrails.MaxEntitlementChecks {
		return nil, apperr.Invalidf("at most 100 entitlements are allowed")
	}
	for _, key := range keys {
		if strings.TrimSpace(key) == "" || len(key) > 256 || !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
			return nil, apperr.Invalidf("invalid entitlement key")
		}
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	customer, err := db.ResolveCustomerID(customerID)
	if err != nil {
		return nil, apperr.Invalidf("invalid customer_id")
	}
	if at.IsZero() {
		at = s.now()
	}
	result := make(map[string]bool, len(keys))
	if len(keys) == 0 {
		return result, nil
	}
	rows, err := s.db.Gen(ctx).CheckResourceEntitlements(ctx, gen.CheckResourceEntitlementsParams{MerchantID: mid.UUID(), CustomerID: customer, Entitlements: keys, AtTime: at})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.Entitlement] = row.HasAccess
	}
	return result, nil
}
