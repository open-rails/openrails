package entitlements

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ValidateCheck refuses an entitlement check before it reads anything: its
// keys, its prefixes and its per-prefix limit.
func ValidateCheck(params billing.CheckEntitlementsParams) error {
	if len(params.Entitlements) == 0 && len(params.Prefixes) == 0 {
		return apperr.Invalidf("entitlements or prefixes is required").WithParam("entitlements")
	}
	if err := validateKeys(params.Entitlements); err != nil {
		return err
	}
	if err := validatePrefixes(params.Prefixes); err != nil {
		return err
	}
	if params.PrefixLimit < 0 || params.PrefixLimit > billing.MaxHeldEntitlements {
		return apperr.Invalidf("prefix_limit must be between 0 and %d", billing.MaxHeldEntitlements).WithParam("prefix_limit")
	}
	return nil
}

func validKey(key string) bool {
	return key != "" && len(key) <= catalog.MaxEntitlementKeyBytes && utf8.ValidString(key) && !strings.ContainsRune(key, 0)
}

func validateKeys(keys []string) error {
	if len(keys) > billing.MaxEntitlementChecks {
		return apperr.Invalidf("at most %d entitlements per check", billing.MaxEntitlementChecks).WithParam("entitlements")
	}
	for _, key := range keys {
		if strings.TrimSpace(key) == "" || !validKey(key) {
			return apperr.Invalidf("invalid entitlement key").WithParam("entitlements")
		}
	}
	return nil
}

// validatePrefixes requires distinct byte prefixes whose last byte is
// printable ASCII, so the byte after it bounds the prefix's range.
func validatePrefixes(prefixes []string) error {
	if len(prefixes) > billing.MaxEntitlementPrefixes {
		return apperr.Invalidf("at most %d prefixes per check", billing.MaxEntitlementPrefixes).WithParam("prefixes")
	}
	for i, prefix := range prefixes {
		if !validKey(prefix) {
			return apperr.Invalidf("a prefix is 1 to %d bytes of UTF-8 without NUL", catalog.MaxEntitlementKeyBytes).WithParam("prefixes")
		}
		if last := prefix[len(prefix)-1]; last < 0x21 || last > 0x7E {
			return apperr.Invalidf("a prefix must end in a printable ASCII byte").WithParam("prefixes")
		}
		if slices.Contains(prefixes[:i], prefix) {
			return apperr.Invalidf("duplicate prefix").WithParam("prefixes")
		}
	}
	return nil
}

// CheckMany resolves only the requested resource keys; it never enumerates a
// customer's complete entitlement history or consults mutable product contents.
func (s *EntitlementService) CheckMany(ctx context.Context, customerID string, keys []string, at time.Time) (map[string]bool, error) {
	if err := validateKeys(keys); err != nil {
		return nil, err
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

// HeldByPrefix answers, for each prefix, the distinct keys the customer holds
// at at under it in byte order: at most limit (zero: the default), Truncated
// when more are held. Every requested prefix is in the result.
func (s *EntitlementService) HeldByPrefix(ctx context.Context, customerID string, prefixes []string, limit int, at time.Time) (map[string]billing.HeldEntitlements, error) {
	if err := validatePrefixes(prefixes); err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = billing.DefaultHeldEntitlements
	}
	if limit < 0 || limit > billing.MaxHeldEntitlements {
		return nil, apperr.Invalidf("prefix_limit must be between 0 and %d", billing.MaxHeldEntitlements).WithParam("prefix_limit")
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
	held := make(map[string]billing.HeldEntitlements, len(prefixes))
	if len(prefixes) == 0 {
		return held, nil
	}
	uppers := make([]string, len(prefixes))
	for i, prefix := range prefixes {
		uppers[i] = prefix[:len(prefix)-1] + string(prefix[len(prefix)-1]+1)
		held[prefix] = billing.HeldEntitlements{Keys: []string{}}
	}
	rows, err := s.db.Gen(ctx).ListHeldEntitlementsByPrefix(ctx, gen.ListHeldEntitlementsByPrefixParams{
		Prefixes: prefixes, Uppers: uppers, MerchantID: mid.UUID(), CustomerID: customer, AtTime: at, RowLimit: int32(limit + 1),
	})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		h := held[row.Prefix]
		h.Keys = append(h.Keys, row.Entitlement)
		held[row.Prefix] = h
	}
	for prefix, h := range held {
		slices.Sort(h.Keys)
		if len(h.Keys) > limit {
			h.Keys, h.Truncated = h.Keys[:limit], true
		}
		held[prefix] = h
	}
	return held, nil
}
