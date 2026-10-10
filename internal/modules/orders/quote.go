package orders

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Quote is lines priced as an order freezes them.
type Quote struct {
	Currency string
	Total    int64
	Lines    []QuotedLine
}

// QuotedLine is one priced line, or why it cannot be bought.
type QuotedLine struct {
	Price   *models.Price
	Product *models.Product
	// Quantity is nil on a recurring line of a price without seats.
	Quantity   *int
	UnitAmount int64
	Amount     int64
	Ownership  catalog.Ownership
	ClaimKey   string
	Credit     *models.CreditGrantSnapshot
	Refusal    *billing.OrderLineRefusal
}

// Refused is the first refused line, -1 when every line can be bought.
func (q *Quote) Refused() int {
	for i, l := range q.Lines {
		if l.Refusal != nil {
			return i
		}
	}
	return -1
}

// Preview is the quote as the customer reads it.
func (q *Quote) Preview(options []billing.OrderPaymentOption) billing.OrderPreview {
	out := billing.OrderPreview{Currency: q.Currency, Total: q.Total, Lines: make([]billing.OrderPreviewLine, 0, len(q.Lines)), PaymentOptions: options}
	if out.PaymentOptions == nil {
		out.PaymentOptions = []billing.OrderPaymentOption{}
	}
	for _, l := range q.Lines {
		line := billing.OrderPreviewLine{Quantity: l.Quantity, UnitAmount: l.UnitAmount, Amount: l.Amount, Ownership: l.Ownership, Refusal: l.Refusal}
		if l.Price != nil {
			line.PriceID, line.BillingIntervalHours, line.AccessDurationHours = billing.PriceID(l.Price.ID), l.Price.BillingIntervalHours, l.Price.AccessDurationHours
		}
		if l.Product != nil {
			line.ProductID, line.Description = billing.ProductID(l.Product.ID), l.Product.DisplayName
		}
		out.Lines = append(out.Lines, line)
	}
	return out
}

// Prices are the buyable lines' prices, for routing.
func (q *Quote) Prices() []*models.Price {
	out := make([]*models.Price, 0, len(q.Lines))
	for _, l := range q.Lines {
		if l.Refusal == nil {
			out = append(out, l.Price)
		}
	}
	return out
}

// Products are the buyable lines' products, beside Prices.
func (q *Quote) Products() []*models.Product {
	out := make([]*models.Product, 0, len(q.Lines))
	for _, l := range q.Lines {
		if l.Refusal == nil {
			out = append(out, l.Product)
		}
	}
	return out
}

func refusal(code, message string) *billing.OrderLineRefusal {
	return &billing.OrderLineRefusal{Code: code, Message: message}
}

// CheckLines refuses a malformed request before anything is read.
func CheckLines(lines []billing.OrderLineParams) error {
	if len(lines) == 0 || len(lines) > billing.MaxOrderLines {
		return apperr.Invalidf("an order has 1 to %d lines", billing.MaxOrderLines).WithParam("lines")
	}
	seen := map[billing.PriceID]bool{}
	for i, l := range lines {
		if l.PriceID.IsZero() {
			return apperr.Invalidf("price_id required").WithParam(fmt.Sprintf("lines[%d].price_id", i))
		}
		if seen[l.PriceID] {
			return apperr.Invalidf("each price is one line; set its quantity").WithParam(fmt.Sprintf("lines[%d].price_id", i))
		}
		seen[l.PriceID] = true
		if l.Quantity != nil && *l.Quantity < 1 {
			return apperr.Invalidf("quantity must be positive").WithParam(fmt.Sprintf("lines[%d].quantity", i))
		}
	}
	return nil
}

// quote prices lines for customer through q (the caller's transaction when
// creating). exceptOrder's own claims do not refuse it.
func (s *Service) quote(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, lines []billing.OrderLineParams, exceptOrder uuid.UUID, at time.Time) (*Quote, error) {
	if err := CheckLines(lines); err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(lines))
	for i, l := range lines {
		ids[i] = l.PriceID.UUID()
	}
	rows, err := q.ListPricesWithProductByIDs(ctx, gen.ListPricesWithProductByIDsParams{MerchantID: merchantID, Ids: ids})
	if err != nil {
		return nil, err
	}
	prices := map[uuid.UUID]*models.Price{}
	products := map[uuid.UUID]*models.Product{}
	for _, row := range rows {
		price, err := models.PriceFromGen(row.BillingPrice)
		if err != nil {
			return nil, err
		}
		product, err := models.ProductFromGen(row.BillingProduct)
		if err != nil {
			return nil, err
		}
		prices[price.ID], products[product.ID] = price, product
	}
	productIDs := make([]uuid.UUID, 0, len(products))
	for id := range products {
		productIDs = append(productIDs, id)
	}
	current, err := q.ListCurrentPricesByProducts(ctx, gen.ListCurrentPricesByProductsParams{MerchantID: merchantID, ProductIds: productIDs})
	if err != nil {
		return nil, err
	}
	sold := map[uuid.UUID][]catalog.OwnershipPrice{}
	for _, p := range current {
		sold[p.ProductID] = append(sold[p.ProductID], catalog.OwnershipPrice{Recurring: p.BillingIntervalHours != nil && *p.BillingIntervalHours > 0, AccessDuration: p.AccessDurationHours != nil})
	}

	out := &Quote{Lines: make([]QuotedLine, len(lines))}
	claims := map[string]int{}
	seenProducts := map[uuid.UUID]int{}
	credits := 0
	for i, l := range lines {
		line := &out.Lines[i]
		line.Quantity = l.Quantity
		price := prices[l.PriceID.UUID()]
		if price == nil {
			line.Refusal = refusal(RefusalUnavailable, "The price does not exist.")
			continue
		}
		product := products[price.ProductID]
		if j, dup := seenProducts[product.ID]; dup {
			return nil, apperr.Invalidf("lines[%d] buys the product lines[%d] buys; set one line's quantity", i, j).WithParam(fmt.Sprintf("lines[%d].price_id", i))
		}
		seenProducts[product.ID] = i
		line.Price, line.Product = price, product
		line.UnitAmount = price.Amount
		line.Ownership = catalog.DeriveOwnership(product.Ownership, product.CreditGrant != nil, sold[product.ID])
		switch {
		case !price.IsPurchasable() || !product.IsPurchasable():
			line.Refusal = refusal(RefusalUnavailable, "The price is not for sale.")
		case price.CustomerAmount != nil:
			line.Refusal = refusal(RefusalUnavailable, "A price whose amount the customer chooses is not sold in an order.")
		case price.TrialUnitAmount != nil || price.TrialDurationHours != nil:
			line.Refusal = refusal(RefusalUnavailable, "A trial price is not sold in an order.")
		case price.IsRecurring() && price.Amount <= 0:
			line.Refusal = refusal(RefusalUnavailable, "A free recurring price is not sold in an order.")
		default:
			line.Quantity, line.Refusal = lineQuantity(price, line.Ownership, l.Quantity)
		}
		if line.Refusal != nil {
			continue
		}
		if out.Currency == "" {
			out.Currency = price.Currency
		} else if !strings.EqualFold(out.Currency, price.Currency) {
			line.Refusal = refusal(RefusalCurrencyMismatch, "The price is in "+price.Currency+"; the order is in "+out.Currency+".")
			continue
		}
		units := int64(1)
		if line.Quantity != nil {
			units = int64(*line.Quantity)
		}
		if line.UnitAmount > 0 && units > math.MaxInt64/line.UnitAmount {
			line.Refusal = refusal(RefusalQuantityInvalid, "The line's amount is too large.")
			continue
		}
		line.Amount = line.UnitAmount * units
		if product.CreditGrant != nil {
			credit, err := creditSnapshot(product, price)
			if err != nil || price.Amount <= 0 || credits > 0 {
				line.Refusal = refusal(RefusalUnavailable, "An order buys at most one credit product, at a price.")
				continue
			}
			credits++
			line.Credit = credit
		}
		if line.Ownership == catalog.OwnershipUnique {
			line.ClaimKey = claimKey(product)
			if j, dup := claims[line.ClaimKey]; dup {
				return nil, apperr.Invalidf("lines[%d] buys what lines[%d] buys", i, j).WithParam(fmt.Sprintf("lines[%d].price_id", i))
			}
			claims[line.ClaimKey] = i
		}
	}
	if err := s.refuseOwned(ctx, q, merchantID, customerID, out, exceptOrder, at); err != nil {
		return nil, err
	}
	for _, l := range out.Lines {
		if l.Refusal == nil {
			if out.Total > math.MaxInt64-l.Amount {
				return nil, apperr.Invalidf("the order's total is too large").WithParam("lines")
			}
			out.Total += l.Amount
		}
	}
	return out, nil
}

// lineQuantity is a line's quantity under its price: seats within a per-seat
// price's bounds (its minimum when omitted), none on another recurring price,
// units of a consumable, and 1 on another one-time price.
func lineQuantity(price *models.Price, ownership catalog.Ownership, asked *int) (*int, *billing.OrderLineRefusal) {
	one := 1
	if price.IsRecurring() {
		low, high, perSeat := SeatBounds(price)
		switch {
		case !perSeat && asked != nil:
			return nil, refusal(RefusalQuantityNotAllowed, "The price has no seats: send no quantity.")
		case !perSeat:
			return nil, nil
		case asked == nil:
			return &low, nil
		case *asked < low || *asked > high:
			return asked, refusal(RefusalQuantityInvalid, fmt.Sprintf("The price sells %d to %d seats.", low, high))
		}
		return asked, nil
	}
	switch {
	case asked == nil:
		return &one, nil
	case *asked > MaxQuantity || *asked > 1 && ownership != catalog.OwnershipConsumable:
		return asked, refusal(RefusalQuantityInvalid, "Only a consumable line takes a quantity above 1.")
	}
	return asked, nil
}

// SeatBounds is a per-seat price's quantity bounds; a recurring price
// without them has no seats.
func SeatBounds(price *models.Price) (low, high int, ok bool) {
	return 0, 0, false
}

// claimKey is the ownership a unique product claims: its tier group, else
// the product itself.
func claimKey(product *models.Product) string {
	if product.TierGroup != nil && strings.TrimSpace(*product.TierGroup) != "" {
		return "tier_group:" + *product.TierGroup
	}
	return "product:" + product.ID.String()
}

// refuseOwned refuses the unique lines the customer already holds: a live
// subscription or never-ending access, or a claim another order holds.
func (s *Service) refuseOwned(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, quote *Quote, exceptOrder uuid.UUID, at time.Time) error {
	var productIDs []uuid.UUID
	var groups, keys []string
	for _, l := range quote.Lines {
		if l.Refusal != nil || l.ClaimKey == "" {
			continue
		}
		productIDs, keys = append(productIDs, l.Product.ID), append(keys, l.ClaimKey)
		if l.Product.TierGroup != nil {
			groups = append(groups, *l.Product.TierGroup)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	held, err := q.ListLiveOwnership(ctx, gen.ListLiveOwnershipParams{MerchantID: merchantID, CustomerID: customerID, ProductIds: productIDs, TierGroups: groups, Now: at})
	if err != nil {
		return err
	}
	claims, err := q.ListOwnershipClaims(ctx, gen.ListOwnershipClaimsParams{MerchantID: merchantID, CustomerID: customerID, ClaimKeys: keys})
	if err != nil {
		return err
	}
	for i := range quote.Lines {
		l := &quote.Lines[i]
		if l.Refusal != nil || l.ClaimKey == "" {
			continue
		}
		for _, h := range held {
			sameGroup := l.Product.TierGroup != nil && h.TierGroup != nil && *h.TierGroup == *l.Product.TierGroup
			if h.ProductID != l.Product.ID && !sameGroup {
				continue
			}
			if h.HolderType == "subscription" {
				hint := "change"
				if h.Status == string(models.StatusCanceled) {
					hint = "resume"
				}
				l.Refusal = owned(billing.SubscriptionID(h.HolderID).String(), &hint, "The customer already has this subscription.")
			} else {
				l.Refusal = owned(billing.ProductAccessID(h.HolderID).String(), nil, "The customer already owns this product.")
			}
			break
		}
		if l.Refusal != nil {
			continue
		}
		for _, c := range claims {
			if c.ClaimKey != l.ClaimKey || c.HolderType == "order" && c.OrderID == exceptOrder {
				continue
			}
			l.Refusal = claimRefusal(c)
		}
	}
	return nil
}

func owned(by string, hint *string, message string) *billing.OrderLineRefusal {
	return &billing.OrderLineRefusal{Code: RefusalAlreadyOwned, Message: message, OwnedBy: &by, Hint: hint}
}

// claimRefusal names the holder of a claim.
func claimRefusal(c gen.BillingOwnershipClaim) *billing.OrderLineRefusal {
	switch c.HolderType {
	case "subscription":
		hint := "change"
		return owned(billing.SubscriptionID(c.HolderID).String(), &hint, "The customer already has this subscription.")
	case "product_access":
		return owned(billing.ProductAccessID(c.HolderID).String(), nil, "The customer already owns this product.")
	}
	hint := "resume"
	return owned(billing.OrderID(c.OrderID).String(), &hint, "Another unpaid order of the customer buys this.")
}

// creditSnapshot is a credit product's frozen benefit for one unit.
func creditSnapshot(product *models.Product, price *models.Price) (*models.CreditGrantSnapshot, error) {
	policy := product.CreditGrant
	if price.IsRecurring() {
		return nil, fmt.Errorf("recurring credit benefits are not supported")
	}
	currency := strings.ToUpper(strings.TrimSpace(policy.Currency))
	if currency != strings.ToUpper(strings.TrimSpace(price.Currency)) {
		return nil, fmt.Errorf("purchased credit currency must match payment currency")
	}
	amount := price.Amount
	if policy.Amount != nil {
		amount = *policy.Amount
	} else if !policy.FromPayment {
		return nil, fmt.Errorf("credit benefit requires an amount or from_payment")
	}
	days := 365
	if policy.ExpiresAfterDays != nil {
		days = *policy.ExpiresAfterDays
	}
	out := &models.CreditGrantSnapshot{Amount: amount, Currency: currency, ExpiresAfterDays: days}
	return out, out.Validate()
}
