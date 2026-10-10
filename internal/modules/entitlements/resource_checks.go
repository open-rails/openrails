package entitlements

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

func validKey(key string) bool {
	return key != "" && len(key) <= catalog.MaxEntitlementKeyBytes && utf8.ValidString(key) && !strings.ContainsRune(key, 0)
}

// validatePrefix requires a byte prefix whose last byte is printable ASCII,
// so the byte after it bounds the prefix's range.
func validatePrefix(prefix string) error {
	if !validKey(prefix) {
		return apperr.Invalidf("a prefix is 1 to %d bytes of UTF-8 without NUL", catalog.MaxEntitlementKeyBytes).WithParam("prefix")
	}
	if last := prefix[len(prefix)-1]; last < 0x21 || last > 0x7E {
		return apperr.Invalidf("a prefix must end in a printable ASCII byte").WithParam("prefix")
	}
	return nil
}

// prefixUpper is the byte after a prefix's range: its last byte plus one.
func prefixUpper(prefix string) string {
	return prefix[:len(prefix)-1] + string(prefix[len(prefix)-1]+1)
}

// MaxEntitlementPage bounds one page of held keys.
const MaxEntitlementPage = 1000

// Held is one key one customer holds, and the most seats a held per-seat
// product grants it (nil: none per seat).
type Held struct {
	Customer uuid.UUID
	Key      string
	Seats    *int
}

func (h Held) less(o Held) bool {
	if c := bytes.Compare(h.Customer[:], o.Customer[:]); c != 0 {
		return c < 0
	}
	return h.Key < o.Key
}

// Query selects held keys. Customers are 1 to billing.MaxBatchItems
// customers; none reads the holders of the one key in Keys. Keys keeps only
// those keys (at most billing.MaxBatchItems), Prefix only keys under that
// byte prefix. At zero is now. After is the last row of the previous page.
type Query struct {
	Customers []uuid.UUID
	Keys      []string
	Prefix    string
	At        time.Time
	After     Held
	Limit     int
}

func (q Query) validate() error {
	if q.Limit <= 0 || q.Limit > MaxEntitlementPage {
		return apperr.Invalidf("limit must be between 1 and %d", MaxEntitlementPage).WithParam("limit")
	}
	if len(q.Customers) > billing.MaxBatchItems {
		return apperr.Invalidf("customer_id names at most %d customers", billing.MaxBatchItems).WithParam("customer_id")
	}
	if len(q.Keys) > billing.MaxBatchItems {
		return apperr.Invalidf("at most %d entitlements per read", billing.MaxBatchItems).WithParam("entitlement")
	}
	for _, key := range q.Keys {
		if strings.TrimSpace(key) == "" || !validKey(key) {
			return apperr.Invalidf("invalid entitlement key").WithParam("entitlement")
		}
	}
	if q.Prefix != "" {
		if err := validatePrefix(q.Prefix); err != nil {
			return err
		}
	}
	if len(q.Customers) == 0 && (len(q.Keys) != 1 || q.Prefix != "") {
		return apperr.Invalidf("without customer_id, name exactly one entitlement").WithParam("entitlement")
	}
	return nil
}

// List answers which keys customers hold at q.At, in (customer, key) byte
// order: one page after q.After, and whether another follows. It reads one
// snapshot. A heavy buyer's keys come from their cache while it is valid at
// q.At; a read at the current instant that derived them live rebuilds it.
func (s *EntitlementService) List(ctx context.Context, q Query) ([]Held, bool, error) {
	if err := q.validate(); err != nil {
		return nil, false, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	current := q.At.IsZero()
	at := q.At
	if current {
		at = s.now()
	}
	if len(q.Customers) == 0 {
		return s.listHolders(ctx, mid.UUID(), q.Keys[0], at, q.After.Customer, q.Limit)
	}
	keys := q.Keys
	if q.Prefix != "" && keys != nil {
		keys = slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return !strings.HasPrefix(k, q.Prefix) })
		if len(keys) == 0 {
			return []Held{}, false, nil
		}
	}
	customers := slices.Clone(q.Customers)
	slices.SortFunc(customers, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	customers = slices.Compact(customers)
	customers = slices.DeleteFunc(customers, func(c uuid.UUID) bool { return bytes.Compare(c[:], q.After.Customer[:]) < 0 })
	if len(customers) == 0 {
		return []Held{}, false, nil
	}
	before := ""
	if q.Prefix != "" {
		before = prefixUpper(q.Prefix)
	}
	limit := int32(q.Limit + 1) // #nosec G115 -- limit is bounded above
	var rows []Held
	var heavy []uuid.UUID
	err = s.db.ReadSnapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		gq := s.db.NewWithPgxTx(tx).Gen(ctx)
		valid, err := gq.ListValidEntitlementCaches(ctx, gen.ListValidEntitlementCachesParams{MerchantID: mid.UUID(), CustomerIds: customers, AtTime: at})
		if err != nil {
			return err
		}
		cached := slices.DeleteFunc(slices.Clone(customers), func(c uuid.UUID) bool { return !slices.Contains(valid, c) })
		derived := slices.DeleteFunc(slices.Clone(customers), func(c uuid.UUID) bool { return slices.Contains(valid, c) })
		if len(derived) > 0 {
			if err := customPlans(ctx, gq); err != nil {
				return err
			}
			if keys != nil {
				found, err := gq.CheckDerivedEntitlements(ctx, gen.CheckDerivedEntitlementsParams{
					MerchantID: mid.UUID(), CustomerIds: derived, Entitlements: keys, AtTime: at, RowLimit: int32(len(derived) * len(keys)), // #nosec G115 -- both are bounded by MaxBatchItems
				})
				if err != nil {
					return err
				}
				for _, f := range found {
					rows = append(rows, Held{f.CustomerID, f.Entitlement, models.SeatsOf(f.Quantity)})
				}
			} else {
				found, err := gq.ListDerivedEntitlementsPage(ctx, gen.ListDerivedEntitlementsPageParams{
					MerchantID: mid.UUID(), CustomerIds: derived, AfterCustomer: q.After.Customer, AfterKey: q.After.Key,
					LowKey: q.Prefix, BeforeKey: before, AtTime: at, RowLimit: limit,
				})
				if err != nil {
					return err
				}
				for _, f := range found {
					rows = append(rows, Held{f.CustomerID, f.Entitlement, models.SeatsOf(f.Quantity)})
				}
			}
			if current {
				if heavy, err = gq.ListHeavyBuyers(ctx, gen.ListHeavyBuyersParams{MerchantID: mid.UUID(), CustomerIds: derived, AtTime: at, UpTo: HeavyBuyerProducts}); err != nil {
					return err
				}
			}
		}
		if len(cached) == 0 {
			return nil
		}
		if err := cachedReads(ctx, gq); err != nil {
			return err
		}
		if keys != nil {
			found, err := gq.CheckCachedEntitlements(ctx, gen.CheckCachedEntitlementsParams{
				MerchantID: mid.UUID(), CustomerIds: cached, Entitlements: keys, RowLimit: int32(len(cached) * len(keys)), // #nosec G115 -- both are bounded by MaxBatchItems
			})
			for _, f := range found {
				rows = append(rows, Held{f.CustomerID, f.Entitlement, models.SeatsOf(f.Quantity)})
			}
			if err != nil {
				return err
			}
		} else {
			found, err := gq.ListCachedEntitlementsPage(ctx, gen.ListCachedEntitlementsPageParams{
				MerchantID: mid.UUID(), CustomerIds: cached, AfterCustomer: q.After.Customer, AfterKey: q.After.Key,
				LowKey: q.Prefix, BeforeKey: before, RowLimit: limit,
			})
			for _, f := range found {
				rows = append(rows, Held{f.CustomerID, f.Entitlement, models.SeatsOf(f.Quantity)})
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	for _, customer := range heavy {
		s.rebuildAfterMiss(mid, customer)
	}
	rows = slices.DeleteFunc(rows, func(h Held) bool { return !q.After.less(h) })
	slices.SortFunc(rows, func(a, b Held) int {
		if a.less(b) {
			return -1
		}
		if b.less(a) {
			return 1
		}
		return 0
	})
	if len(rows) > q.Limit {
		return rows[:q.Limit], true, nil
	}
	return rows, false, nil
}

// listHolders is the reverse lookup: customers holding key at at, after
// afterID (uuid.Nil starts), in id order.
func (s *EntitlementService) listHolders(ctx context.Context, mid uuid.UUID, key string, at time.Time, afterID uuid.UUID, limit int) ([]Held, bool, error) {
	var holders []gen.ListDerivedEntitlementHoldersRow
	err := s.db.ReadSnapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.db.NewWithPgxTx(tx).Gen(ctx)
		if err := customPlans(ctx, q); err != nil {
			return err
		}
		var err error
		holders, err = q.ListDerivedEntitlementHolders(ctx, gen.ListDerivedEntitlementHoldersParams{
			MerchantID: mid, Entitlement: key, AtTime: at, AfterID: afterID, RowLimit: int32(limit + 1), // #nosec G115 -- bounded above
		})
		return err
	})
	if err != nil {
		return nil, false, err
	}
	out := make([]Held, 0, len(holders))
	for _, c := range holders {
		out = append(out, Held{c.CustomerID, key, models.SeatsOf(c.Quantity)})
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// ListEntitlementSources explains why the customer holds each key at at:
// every live window of a product granting it.
func (s *EntitlementService) ListEntitlementSources(ctx context.Context, customer uuid.UUID, keys []string, at time.Time) ([]gen.ListEntitlementSourcesRow, error) {
	if len(keys) == 0 {
		return nil, apperr.Invalidf("entitlements is required").WithParam("entitlements")
	}
	if err := (Query{Customers: []uuid.UUID{customer}, Keys: keys, Limit: 1}).validate(); err != nil {
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
