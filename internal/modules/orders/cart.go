package orders

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// The cart: one per customer of a merchant, lines the quote engine prices on
// every read. Every change goes through changeCart in one transaction.

// cartLine is one stored line.
type cartLine struct {
	PriceID  uuid.UUID `json:"price_id"`
	Quantity *int      `json:"quantity,omitempty"`
	Amount   *int64    `json:"amount,omitempty,string"`
}

// Cart is a customer's cart: its lines in the order added, and when it last
// changed (zero while it is empty).
type Cart struct {
	Lines         []billing.OrderLineParams
	LastChangedAt time.Time
}

// ExpiresAt is when the cart expires unless it changes first.
func (c Cart) ExpiresAt() time.Time { return c.LastChangedAt.Add(retention.Carts) }

func cartOf(lines []cartLine, changed time.Time) Cart {
	out := Cart{Lines: make([]billing.OrderLineParams, 0, len(lines))}
	for _, l := range lines {
		out.Lines = append(out.Lines, billing.OrderLineParams{PriceID: billing.PriceID(l.PriceID), Quantity: l.Quantity, Amount: l.Amount})
	}
	if len(lines) > 0 {
		out.LastChangedAt = changed
	}
	return out
}

// cartLines decodes a stored cart; an expired one is empty.
func cartLines(raw []byte, changed, now time.Time) ([]cartLine, error) {
	if !changed.After(now.Add(-retention.Carts)) {
		return nil, nil
	}
	var lines []cartLine
	return lines, json.Unmarshal(raw, &lines)
}

// Cart reads the customer's cart.
func (s *Service) Cart(ctx context.Context, customerID uuid.UUID) (Cart, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return Cart{}, err
	}
	row, err := s.DB.Gen(ctx).GetCart(ctx, gen.GetCartParams{MerchantID: mid.UUID(), CustomerID: customerID})
	if db.IsNotFound(err) {
		return cartOf(nil, time.Time{}), nil
	}
	if err != nil {
		return Cart{}, err
	}
	lines, err := cartLines(row.Lines, row.LastChangedAt, s.now())
	if err != nil {
		return Cart{}, err
	}
	return cartOf(lines, row.LastChangedAt), nil
}

// SetCartLine puts price in the customer's cart: quantity 0 removes its line,
// and a price of a product already in the cart takes that line's place.
func (s *Service) SetCartLine(ctx context.Context, customerID, priceID uuid.UUID, quantity *int, amount *int64) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	remove := quantity != nil && *quantity == 0
	switch {
	case quantity != nil && *quantity < 0:
		return apperr.Invalidf("quantity is 0 (remove the line) or more").WithParam("quantity")
	case remove && amount != nil:
		return apperr.Invalidf("quantity 0 removes the line; send no amount").WithParam("amount")
	}
	return s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.DB.NewWithPgxTx(tx).Gen(ctx)
		if err := db.EnsureCustomerRowQ(ctx, q, mid.UUID(), customerID); err != nil {
			return err
		}
		return s.changeCart(ctx, q, mid.UUID(), customerID, func(lines []cartLine) ([]cartLine, error) {
			if remove {
				return slices.DeleteFunc(lines, func(l cartLine) bool { return l.PriceID == priceID }), nil
			}
			ids := []uuid.UUID{priceID}
			for _, l := range lines {
				ids = append(ids, l.PriceID)
			}
			rows, err := q.ListPricesWithProductByIDs(ctx, gen.ListPricesWithProductByIDsParams{MerchantID: mid.UUID(), Ids: ids})
			if err != nil {
				return nil, err
			}
			productOf := map[uuid.UUID]uuid.UUID{}
			for _, row := range rows {
				productOf[row.BillingPrice.ID] = row.BillingProduct.ID
			}
			product, ok := productOf[priceID]
			if !ok {
				return nil, apperr.New(http.StatusNotFound, billing.CodeResourceNotFound, "price not found")
			}
			line := cartLine{PriceID: priceID, Quantity: quantity, Amount: amount}
			at := slices.IndexFunc(lines, func(l cartLine) bool { return l.PriceID == priceID || productOf[l.PriceID] == product })
			if at < 0 {
				if len(lines) >= billing.MaxCartLines {
					return nil, apperr.Invalidf("a cart has at most %d lines", billing.MaxCartLines).WithParam("price_id")
				}
				return append(lines, line), nil
			}
			lines[at] = line
			kept := lines[:0]
			for i, l := range lines {
				if i == at || l.PriceID != priceID && productOf[l.PriceID] != product {
					kept = append(kept, l)
				}
			}
			return kept, nil
		})
	})
}

// ClearCart empties the customer's cart.
func (s *Service) ClearCart(ctx context.Context, customerID uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.DB.NewWithPgxTx(tx).Gen(ctx)
		if _, err := q.GetCart(ctx, gen.GetCartParams{MerchantID: mid.UUID(), CustomerID: customerID}); db.IsNotFound(err) {
			return nil
		} else if err != nil {
			return err
		}
		return s.changeCart(ctx, q, mid.UUID(), customerID, func([]cartLine) ([]cartLine, error) { return nil, nil })
	})
}

// takeCart empties the customer's cart into an order being created in q's
// transaction and returns its lines.
func (s *Service) takeCart(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID) ([]billing.OrderLineParams, error) {
	var taken []cartLine
	err := s.changeCart(ctx, q, merchantID, customerID, func(lines []cartLine) ([]cartLine, error) {
		taken = lines
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	if len(taken) == 0 {
		return nil, apperr.Invalidf("the cart is empty").WithParam("cart")
	}
	return cartOf(taken, time.Time{}).Lines, nil
}

// changeCart is the one way a cart changes: under the cart's row lock, in
// q's transaction, edit turns its lines into the new ones, written with
// last_changed_at. An expired cart edits as empty.
func (s *Service) changeCart(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, edit func([]cartLine) ([]cartLine, error)) error {
	now := s.now()
	row, err := q.LockCart(ctx, gen.LockCartParams{MerchantID: merchantID, CustomerID: customerID, Now: now})
	if err != nil {
		return err
	}
	lines, err := cartLines(row.Lines, row.LastChangedAt, now)
	if err != nil {
		return err
	}
	next, err := edit(lines)
	if err != nil {
		return err
	}
	if next == nil {
		next = []cartLine{}
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return q.SetCartLines(ctx, gen.SetCartLinesParams{MerchantID: merchantID, CustomerID: customerID, Lines: raw, Now: now})
}

// DeleteExpiredCarts deletes up to limit carts unchanged for retention.Carts,
// across merchants.
func DeleteExpiredCarts(ctx context.Context, database *db.DB, now time.Time, limit int32) (int64, error) {
	return database.GenDirectory().DeleteExpiredCarts(ctx, gen.DeleteExpiredCartsParams{Before: now.Add(-retention.Carts), RowLimit: limit})
}
