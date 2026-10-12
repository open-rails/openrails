package orders

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// RefusalEntitlementRequired is also the line's error code.
const RefusalEntitlementRequired = billing.CodeEntitlementRequired

// refuseUnentitled refuses each buyable line whose product requires keys the
// customer does not hold at at.
func refuseUnentitled(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, lines []QuotedLine, at time.Time) error {
	var keys []string
	for _, l := range lines {
		if l.Refusal == nil && l.Product != nil {
			keys = append(keys, l.Product.RequiredEntitlements...)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	held, err := q.CheckDerivedEntitlements(ctx, gen.CheckDerivedEntitlementsParams{MerchantID: merchantID, CustomerIds: []uuid.UUID{customerID},
		Entitlements: keys, AtTime: at, RowLimit: int32(len(keys))}) // #nosec G115 -- at most MaxOrderLines × catalog.MaxRequiredEntitlements
	if err != nil {
		return err
	}
	holds := make(map[string]bool, len(held))
	for _, h := range held {
		holds[h.Entitlement] = true
	}
	for i := range lines {
		l := &lines[i]
		if l.Refusal != nil || l.Product == nil {
			continue
		}
		var missing []string
		for _, key := range l.Product.RequiredEntitlements {
			if !holds[key] {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			l.Refusal = &billing.OrderLineRefusal{Code: RefusalEntitlementRequired, Missing: missing,
				Message: "The product sells only to customers who hold " + strings.Join(missing, ", ") + "."}
		}
	}
	return nil
}

// entitled refuses an order line whose product requires keys the customer
// no longer holds at at. Only purchases are checked: renewals and usage are
// not new sales.
func entitled(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, origin string, lines []gen.BillingOrderLine, at time.Time) error {
	if origin != string(billing.OrderOriginCustomer) && origin != string(billing.OrderOriginMerchant) {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(lines))
	for _, l := range lines {
		if l.ProductID != nil {
			ids = append(ids, *l.ProductID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.ListProductsByIDs(ctx, gen.ListProductsByIDsParams{MerchantID: merchantID, Ids: ids})
	if err != nil {
		return err
	}
	products := make(map[uuid.UUID]*models.Product, len(rows))
	for _, row := range rows {
		p, err := models.ProductFromGen(row)
		if err != nil {
			return err
		}
		products[p.ID] = p
	}
	quoted := make([]QuotedLine, len(lines))
	for i, l := range lines {
		if l.ProductID != nil {
			quoted[i].Product = products[*l.ProductID]
		}
	}
	if err := refuseUnentitled(ctx, q, merchantID, customerID, quoted, at); err != nil {
		return err
	}
	for i, l := range quoted {
		if l.Refusal != nil {
			return &LineError{Index: int(lines[i].Position), Refusal: *l.Refusal}
		}
	}
	return nil
}
