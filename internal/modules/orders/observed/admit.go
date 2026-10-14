package observed

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

func admit(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, b Bill, now time.Time) (gen.BillingOrder, bool, error) {
	differentAccount := false
	if b.SubscriptionID != nil {
		sub, err := q.GetSubscriptionByID(ctx, gen.GetSubscriptionByIDParams{MerchantID: merchantID, ID: *b.SubscriptionID})
		if err != nil {
			return gen.BillingOrder{}, false, err
		}
		if sub.CustomerID != b.CustomerID || sub.CollectionPolicy != b.Owner {
			return gen.BillingOrder{}, false, ErrConflict
		}
		differentAccount = sub.PspID != b.PSPID
		if differentAccount && (b.Owner != OwnerEngine || b.Kind != "renewal" || b.SourceKind != "accepted_renewal" || b.ExistingOrderID == nil || b.Payment == nil) {
			return gen.BillingOrder{}, false, ErrConflict
		}
	}
	if b.ExistingOrderID != nil {
		order, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: merchantID, ID: *b.ExistingOrderID})
		if err != nil {
			return order, false, err
		}
		if order.CustomerID != b.CustomerID || order.Currency != b.Currency || order.Total != b.Total || order.PspID != nil && *order.PspID != b.PSPID || order.SubscriptionID != nil && !sameID(order.SubscriptionID, b.SubscriptionID) {
			return order, false, ErrConflict
		}
		if differentAccount {
			if err := platformRenewalAttempt(ctx, q, merchantID, order, b); err != nil {
				return order, false, err
			}
		}
		lines, err := q.ListOrderLines(ctx, gen.ListOrderLinesParams{MerchantID: merchantID, OrderIds: []uuid.UUID{order.ID}})
		if err != nil {
			return order, false, err
		}
		if len(lines) != len(b.Lines) {
			return order, false, ErrConflict
		}
		for i, l := range lines {
			x := b.Lines[i]
			if !sameID(l.PriceID, x.PriceID) || !sameID(l.ProductID, x.ProductID) || l.Amount != x.Amount || l.UnitAmount != x.UnitAmount || !sameQuantity(l.Quantity, x.Quantity) || models.DerefStr(l.Source) != x.Source {
				return order, false, ErrConflict
			}
		}
		n, err := q.BindObservedOrderOwner(ctx, gen.BindObservedOrderOwnerParams{MerchantID: merchantID, OrderID: order.ID, CollectionOwner: b.Owner, PspID: b.PSPID})
		if err != nil {
			return order, false, err
		}
		if n != 1 {
			return order, false, ErrConflict
		}
		order.CollectionOwner, order.PspID = b.Owner, &b.PSPID
		return order, false, nil
	}
	if err := db.EnsureCustomerRowQ(ctx, q, merchantID, b.CustomerID); err != nil {
		return gen.BillingOrder{}, false, err
	}
	params := gen.CreateObservedOrderParams{MerchantID: merchantID, ID: uuidutil.NewV7(), CustomerID: b.CustomerID, Origin: "subscription", Currency: b.Currency, Total: b.Total, PspID: b.PSPID, CollectionOwner: b.Owner, SubscriptionBillKind: &b.Kind, SubscriptionID: b.SubscriptionID, PeriodStartsAt: &b.PeriodStart, PeriodEndsAt: &b.PeriodEnd, Now: now}
	due := b.DueAt
	if due.IsZero() {
		due = b.PeriodStart
	}
	params.DueAt = &due
	if b.Kind == "purchase" {
		params.Origin = "merchant"
		params.SubscriptionBillKind = nil
		params.PeriodStartsAt = nil
		params.PeriodEndsAt = nil
		params.DueAt = nil
		if b.Owner == OwnerEngine {
			expiry := now.Add(24 * time.Hour)
			params.ExpiresAt = &expiry
		}
	}
	order, err := q.CreateObservedOrder(ctx, params)
	if err != nil {
		return order, false, err
	}
	for i, line := range b.Lines {
		if err := createLine(ctx, q, order, b.SubscriptionID, i, line, now); err != nil {
			return order, false, err
		}
	}
	return order, true, nil
}

// A platform renewal can settle on its accepted attempt's account. An observed
// provider bill or another engine operation cannot relax subscription binding.
func platformRenewalAttempt(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, order gen.BillingOrder, b Bill) error {
	if order.AttemptID == nil || !sameID(order.SubscriptionID, b.SubscriptionID) || order.PeriodStartsAt == nil || !order.PeriodStartsAt.Equal(b.PeriodStart) || order.PeriodEndsAt == nil || !order.PeriodEndsAt.Equal(b.PeriodEnd) {
		return ErrConflict
	}
	attempt, err := q.GetOrderAttemptByID(ctx, gen.GetOrderAttemptByIDParams{MerchantID: merchantID, ID: *order.AttemptID})
	if err != nil {
		return err
	}
	if attempt.OrderID == nil || *attempt.OrderID != order.ID || attempt.CustomerID != b.CustomerID || attempt.PspID != b.PSPID || attempt.Amount == nil || *attempt.Amount != b.Total || attempt.Currency == nil || *attempt.Currency != b.Currency || attempt.Rail != string(b.Payment.Rail) {
		return ErrConflict
	}
	in, err := q.GetProviderIntent(ctx, gen.GetProviderIntentParams{MerchantID: merchantID, ID: attempt.ID})
	if err != nil {
		return err
	}
	if in.IntentType != "subscription_collection" || !sameID(in.SubscriptionID, b.SubscriptionID) || in.PspID == nil || *in.PspID != b.PSPID || in.Rail != attempt.Rail || in.CustodianID == nil {
		return ErrConflict
	}
	custodian, err := q.GetCustodian(ctx, gen.GetCustodianParams{MerchantID: merchantID, ID: *in.CustodianID})
	if err != nil {
		return err
	}
	if custodian.Kind != models.CustodianPlatform {
		return ErrConflict
	}
	return nil
}

func createLine(ctx context.Context, q *gen.Queries, o gen.BillingOrder, sub *uuid.UUID, position int, l Line, now time.Time) error {
	if l.Source == "" {
		price, err := q.GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: o.MerchantID, ID: *l.PriceID})
		if err != nil {
			return err
		}
		if price.ProductID != *l.ProductID {
			return ErrConflict
		}
	}
	if err := freezeLineOwnership(ctx, q, o.MerchantID, &l, sub != nil); err != nil {
		return err
	}
	quantity, err := int32Ptr(l.Quantity)
	if err != nil {
		return err
	}
	interval, err := int32Ptr(l.BillingIntervalHours)
	if err != nil {
		return err
	}
	// A calendar schedule has no fixed hourly interval. Its implicit single
	// catalog unit is still quantified; never turn a month into fixed hours.
	if quantity == nil && interval == nil && l.Source == "" {
		quantity = new(int32(1))
	}
	access, err := int32Ptr(l.AccessDurationHours)
	if err != nil {
		return err
	}
	position32, err := int32Value(position)
	if err != nil {
		return err
	}
	return q.CreateObservedOrderLine(ctx, gen.CreateObservedOrderLineParams{MerchantID: o.MerchantID, ID: uuidutil.NewV7(), OrderID: o.ID, CustomerID: o.CustomerID, Position: position32, PriceID: l.PriceID, ProductID: l.ProductID, Description: l.Description, Quantity: quantity, UnitAmount: l.UnitAmount, Amount: l.Amount, Ownership: optional(l.Ownership), ClaimKey: optional(l.ClaimKey), BillingIntervalHours: interval, AccessDurationHours: access, ProviderAccessDefault: l.ProviderAccessDefault, SubscriptionID: sub, Source: optional(l.Source), Now: now})
}

// freezeLineOwnership resolves ownership once at acceptance. An accepted
// schedule supplies recurring=true even when its cadence is calendar months.
func freezeLineOwnership(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, l *Line, recurring bool) error {
	if l.Source != "" {
		return nil
	}
	if l.PriceID == nil || l.ProductID == nil {
		return errors.New("accepted catalog line requires price and product")
	}
	if l.Source == "" && l.Ownership == "" {
		p, err := q.GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: merchantID, ID: *l.ProductID})
		if err != nil {
			return err
		}
		l.Ownership = string(catalog.DeriveOwnership(catalog.Ownership(models.DerefStr(p.Ownership)), len(p.CreditGrant) > 0 && string(p.CreditGrant) != "null", []catalog.OwnershipPrice{{Recurring: recurring || l.BillingIntervalHours != nil, AccessDuration: l.AccessDurationHours != nil}}))
		if l.Ownership == "unique" {
			l.ClaimKey = "product:" + p.ID.String()
			if p.TierGroup != nil && strings.TrimSpace(*p.TierGroup) != "" {
				l.ClaimKey = "tier_group:" + *p.TierGroup
			}
		}
	}
	if l.Source == "" && l.Ownership == "unique" && l.ClaimKey == "" {
		return errors.New("unique provider line requires accepted claim key")
	}
	return nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func int32Ptr(n *int) (*int32, error) {
	if n == nil {
		return nil, nil
	}
	if *n < 1 || *n > math.MaxInt32 {
		return nil, errors.New("provider line integer is out of range")
	}
	value, err := int32Value(*n)
	if err != nil {
		return nil, errors.New("provider line integer is out of range")
	}
	return &value, nil
}

func int32Value(n int) (int32, error) {
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, errors.New("provider integer is out of range")
	}
	return int32(n), nil
}

// Flat-price purchase omits quantity, while providers report one unit. These
// represent the same accepted line; money and product identity still match above.
func sameQuantity[T ~int | ~int32](a *T, b *int) bool {
	left, right := int64(1), int64(1)
	if a != nil {
		left = int64(*a)
	}
	if b != nil {
		right = int64(*b)
	}
	return left == right
}
