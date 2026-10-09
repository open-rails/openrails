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
// keys and the keys held under each prefix see the same catalog and access. A
// heavy buyer's keys come from their cache while it is valid at At; a check at
// the current instant that derived them live rebuilds it.
func (s *EntitlementService) Check(ctx context.Context, customerID string, params billing.CheckEntitlementsParams) (billing.EntitlementCheck, error) {
	var out billing.EntitlementCheck
	if err := ValidateCheck(params); err != nil {
		return out, err
	}
	at := params.At
	if at.IsZero() {
		at = s.now()
	}
	limit := params.PrefixLimit
	if limit == 0 {
		limit = billing.DefaultHeldEntitlements
	}
	if _, err := db.ResolveCustomerID(customerID); err != nil {
		return out, apperr.Invalidf("invalid customer_id")
	}
	err := s.withKeys(ctx, customerID, at, params.At.IsZero(), func(ctx context.Context, r readSnapshot) error {
		var err error
		if out.Entitlements, err = checkKeys(ctx, r, params.Entitlements); err != nil {
			return err
		}
		out.Held, err = heldByPrefix(ctx, r, params.Prefixes, limit)
		return err
	})
	return out, err
}

func checkKeys(ctx context.Context, r readSnapshot, keys []string) (map[string]bool, error) {
	result := make(map[string]bool, len(keys))
	if len(keys) == 0 {
		return result, nil
	}
	if r.cached {
		rows, err := r.q.CheckCachedEntitlements(ctx, gen.CheckCachedEntitlementsParams{MerchantID: r.merchant, CustomerID: r.customer, Entitlements: keys})
		for _, row := range rows {
			result[row.Entitlement] = row.HasAccess
		}
		return result, err
	}
	rows, err := r.q.CheckDerivedEntitlements(ctx, gen.CheckDerivedEntitlementsParams{MerchantID: r.merchant, CustomerID: r.customer, Entitlements: keys, AtTime: r.at})
	for _, row := range rows {
		result[row.Entitlement] = row.HasAccess
	}
	return result, err
}

// heldByPrefix answers, for each prefix, the distinct keys held under it in
// byte order: at most limit, Truncated when more are held. Every requested
// prefix is in the result.
func heldByPrefix(ctx context.Context, r readSnapshot, prefixes []string, limit int) (map[string]billing.HeldEntitlements, error) {
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
	type row struct{ prefix, key string }
	var rows []row
	if r.cached {
		cached, err := r.q.ListCachedEntitlementsByPrefix(ctx, gen.ListCachedEntitlementsByPrefixParams{
			MerchantID: r.merchant, CustomerID: r.customer, Prefixes: prefixes, Uppers: uppers, RowLimit: int32(limit + 1), // #nosec G115 -- limit is validated
		})
		if err != nil {
			return nil, err
		}
		for _, c := range cached {
			rows = append(rows, row{c.Prefix, c.Entitlement})
		}
	} else {
		derived, err := r.q.ListDerivedEntitlementsByPrefix(ctx, gen.ListDerivedEntitlementsByPrefixParams{
			MerchantID: r.merchant, CustomerID: r.customer, AtTime: r.at, Low: low, High: high,
			Prefixes: prefixes, Uppers: uppers, RowLimit: int32(limit + 1), // #nosec G115 -- limit is validated
		})
		if err != nil {
			return nil, err
		}
		for _, d := range derived {
			rows = append(rows, row{d.Prefix, d.Entitlement})
		}
	}
	for _, x := range rows {
		h := held[x.prefix]
		h.Keys = append(h.Keys, x.key)
		held[x.prefix] = h
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

// ListEntitlementsPage returns up to limit keys the customer holds at at
// (zero: now), in byte order, after after (exclusive; "" starts), optionally
// under prefix. more: another page follows.
func (s *EntitlementService) ListEntitlementsPage(ctx context.Context, customer uuid.UUID, prefix, after string, limit int, at time.Time) (keys []string, more bool, err error) {
	if limit <= 0 || limit > MaxEntitlementPage {
		return nil, false, apperr.Invalidf("limit must be between 1 and %d", MaxEntitlementPage).WithParam("limit")
	}
	if prefix != "" {
		if err := validatePrefixes([]string{prefix}); err != nil {
			return nil, false, err
		}
	}
	current := at.IsZero()
	if current {
		at = s.now()
	}
	before := ""
	if prefix != "" {
		before = prefixUpper(prefix)
	}
	err = s.withKeys(ctx, customer.String(), at, current, func(ctx context.Context, r readSnapshot) error {
		var err error
		if r.cached {
			keys, err = r.q.ListCachedEntitlementsPage(ctx, gen.ListCachedEntitlementsPageParams{
				MerchantID: r.merchant, CustomerID: customer, LowKey: prefix, AfterKey: after, BeforeKey: before, RowLimit: int32(limit + 1), // #nosec G115 -- limit is bounded above
			})
			return err
		}
		keys, err = r.q.ListDerivedEntitlementsPage(ctx, gen.ListDerivedEntitlementsPageParams{
			MerchantID: r.merchant, CustomerID: customer, AtTime: at, LowKey: prefix, AfterKey: after, BeforeKey: before, RowLimit: int32(limit + 1), // #nosec G115 -- limit is bounded above
		})
		return err
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
	var holders []uuid.UUID
	err = s.db.ReadSnapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.db.NewWithPgxTx(tx).Gen(ctx)
		if err := customPlans(ctx, q); err != nil {
			return err
		}
		holders, err = q.ListDerivedEntitlementHolders(ctx, gen.ListDerivedEntitlementHoldersParams{
			MerchantID: mid.UUID(), Entitlement: entitlement, AtTime: at, AfterID: afterID, RowLimit: int32(limit), // #nosec G115 -- bounded above
		})
		return err
	})
	return holders, err
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
