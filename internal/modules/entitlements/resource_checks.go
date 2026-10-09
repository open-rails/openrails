package entitlements

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

// Check answers an entitlement check in one read-only snapshot: the exact
// keys and the keys held under each prefix see the same catalog and access.
func (s *EntitlementService) Check(ctx context.Context, customerID string, params billing.CheckEntitlementsParams) (billing.EntitlementCheck, error) {
	var out billing.EntitlementCheck
	if err := ValidateCheck(params); err != nil {
		return out, err
	}
	at := params.At
	if at.IsZero() {
		at = s.now()
	}
	err := s.db.ReadSnapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		snapshot := s.db.NewWithPgxTx(tx)
		var err error
		if out.Entitlements, err = s.checkKeys(ctx, snapshot, customerID, params.Entitlements, at); err != nil {
			return err
		}
		out.Held, err = s.heldByPrefix(ctx, snapshot, customerID, params.Prefixes, params.PrefixLimit, at)
		return err
	})
	return out, err
}

// CheckMany answers which of the keys the customer holds at at. It reads the
// products granting each key and the customer's windows of those products.
func (s *EntitlementService) CheckMany(ctx context.Context, customerID string, keys []string, at time.Time) (map[string]bool, error) {
	if err := validateKeys(keys); err != nil {
		return nil, err
	}
	if at.IsZero() {
		at = s.now()
	}
	return s.checkKeys(ctx, s.db, customerID, keys, at)
}

func (s *EntitlementService) checkKeys(ctx context.Context, database *db.DB, customerID string, keys []string, at time.Time) (map[string]bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	customer, err := db.ResolveCustomerID(customerID)
	if err != nil {
		return nil, apperr.Invalidf("invalid customer_id")
	}
	result := make(map[string]bool, len(keys))
	if len(keys) == 0 {
		return result, nil
	}
	rows, err := database.Gen(ctx).CheckDerivedEntitlements(ctx, gen.CheckDerivedEntitlementsParams{MerchantID: mid.UUID(), CustomerID: customer, Entitlements: keys, AtTime: at})
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
	if at.IsZero() {
		at = s.now()
	}
	return s.heldByPrefix(ctx, s.db, customerID, prefixes, limit, at)
}

func (s *EntitlementService) heldByPrefix(ctx context.Context, database *db.DB, customerID string, prefixes []string, limit int, at time.Time) (map[string]billing.HeldEntitlements, error) {
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
	held := make(map[string]billing.HeldEntitlements, len(prefixes))
	if len(prefixes) == 0 {
		return held, nil
	}
	uppers := make([]string, len(prefixes))
	low, high := "", ""
	for i, prefix := range prefixes {
		uppers[i] = prefixUpper(prefix)
		held[prefix] = billing.HeldEntitlements{Keys: []string{}}
		if i == 0 || prefix < low {
			low = prefix
		}
		if i == 0 || uppers[i] > high {
			high = uppers[i]
		}
	}
	rows, err := database.Gen(ctx).ListDerivedEntitlementsByPrefix(ctx, gen.ListDerivedEntitlementsByPrefixParams{
		MerchantID: mid.UUID(), CustomerID: customer, AtTime: at, Low: low, High: high,
		Prefixes: prefixes, Uppers: uppers, RowLimit: int32(limit + 1),
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

// prefixUpper is the byte after a prefix's range: its last byte plus one.
func prefixUpper(prefix string) string {
	return prefix[:len(prefix)-1] + string(prefix[len(prefix)-1]+1)
}

// MaxEntitlementPage bounds one page of a customer's keys or a key's holders.
const MaxEntitlementPage = 1000

// ListEntitlementsPage returns up to limit keys the customer holds at at, in
// byte order, after after (exclusive; "" starts), optionally under prefix.
// more: another page follows.
func (s *EntitlementService) ListEntitlementsPage(ctx context.Context, customer uuid.UUID, prefix, after string, limit int, at time.Time) (keys []string, more bool, err error) {
	if limit <= 0 || limit > MaxEntitlementPage {
		return nil, false, apperr.Invalidf("limit must be between 1 and %d", MaxEntitlementPage).WithParam("limit")
	}
	if prefix != "" {
		if err := validatePrefixes([]string{prefix}); err != nil {
			return nil, false, err
		}
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	if at.IsZero() {
		at = s.now()
	}
	before := ""
	if prefix != "" {
		before = prefixUpper(prefix)
	}
	keys, err = s.db.Gen(ctx).ListDerivedEntitlementsPage(ctx, gen.ListDerivedEntitlementsPageParams{
		MerchantID: mid.UUID(), CustomerID: customer, AtTime: at, LowKey: prefix, AfterKey: after, BeforeKey: before, RowLimit: int32(limit + 1),
	})
	if err != nil {
		return nil, false, err
	}
	if len(keys) > limit {
		return keys[:limit], true, nil
	}
	return keys, false, nil
}

// ListCustomersWithEntitlement is the reverse lookup: customers holding the
// key at at, after afterID (uuid.Nil starts), at most limit, in id order.
func (s *EntitlementService) ListCustomersWithEntitlement(ctx context.Context, entitlement string, at time.Time, afterID uuid.UUID, limit int) ([]uuid.UUID, error) {
	if !validKey(entitlement) || strings.TrimSpace(entitlement) == "" {
		return nil, apperr.Invalidf("invalid entitlement").WithParam("entitlement")
	}
	if limit <= 0 || limit > MaxEntitlementPage+1 {
		limit = MaxEntitlementPage + 1
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if at.IsZero() {
		at = s.now()
	}
	return s.db.Gen(ctx).ListDerivedEntitlementHolders(ctx, gen.ListDerivedEntitlementHoldersParams{
		MerchantID: mid.UUID(), Entitlement: entitlement, AtTime: at, AfterID: afterID, RowLimit: int32(limit),
	})
}

// ListEntitlementSources explains why the customer holds each key at at:
// every live window of a product granting it.
func (s *EntitlementService) ListEntitlementSources(ctx context.Context, customer uuid.UUID, keys []string, at time.Time) ([]gen.ListEntitlementSourcesRow, error) {
	if len(keys) == 0 {
		return nil, apperr.Invalidf("entitlements is required").WithParam("entitlements")
	}
	if err := validateKeys(keys); err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if at.IsZero() {
		at = s.now()
	}
	return s.db.Gen(ctx).ListEntitlementSources(ctx, gen.ListEntitlementSourcesParams{MerchantID: mid.UUID(), CustomerID: customer, Entitlements: keys, AtTime: at})
}
