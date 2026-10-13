package service

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// The customer's cart: lines kept across the merchant's sites, priced live as
// an order preview, and bought with cart: true.

// checkOrderLines refuses a request naming both or neither of lines and the
// cart.
func checkOrderLines(lines []billing.OrderLineParams, cart bool) error {
	switch {
	case cart && len(lines) > 0:
		return apperr.Invalidf("send lines or cart: true, not both").WithParam("cart")
	case cart:
		return nil
	}
	return orders.CheckLines(lines)
}

// orderLinesOf is what an order request buys: its lines, or the customer's
// cart.
func orderLinesOf(ctx context.Context, rt *orderRuntime, customer billing.CustomerID, lines []billing.OrderLineParams, cart bool) ([]billing.OrderLineParams, error) {
	if err := checkOrderLines(lines, cart); err != nil || !cart {
		return lines, err
	}
	c, err := rt.Orders.Cart(ctx, customer.UUID())
	if err != nil {
		return nil, err
	}
	if len(c.Lines) == 0 {
		return nil, apperr.Invalidf("the cart is empty").WithParam("cart")
	}
	return c.Lines, nil
}

// GetCart reads the customer's cart, priced now.
func (s *Service) GetCart(ctx context.Context, actor OrderActor) (*billing.Cart, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, err
	}
	return s.cartView(ctx, rt, actor.Customer)
}

// SetCartLine puts a price in the customer's cart, changes its line, or
// (quantity 0) removes it, and answers the cart.
func (s *Service) SetCartLine(ctx context.Context, actor OrderActor, price billing.PriceID, params billing.SetCartLineParams) (*billing.Cart, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, err
	}
	resolved, err := s.rt.PriceService.GetByPublicID(ctx, price)
	if err != nil {
		return nil, priceLookup(err)
	}
	if err := rt.Orders.SetCartLine(ctx, actor.Customer.UUID(), resolved.ID, params.Quantity, params.Amount); err != nil {
		return nil, err
	}
	return s.cartView(ctx, rt, actor.Customer)
}

// ClearCart empties the customer's cart.
func (s *Service) ClearCart(ctx context.Context, actor OrderActor) error {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return err
	}
	return rt.Orders.ClearCart(ctx, actor.Customer.UUID())
}

// cartView prices the customer's cart as an order preview; a line whose
// price is gone keeps its price id, refused.
func (s *Service) cartView(ctx context.Context, rt *orderRuntime, customer billing.CustomerID) (*billing.Cart, error) {
	cart, err := rt.Orders.Cart(ctx, customer.UUID())
	if err != nil {
		return nil, err
	}
	out := &billing.Cart{Lines: []billing.OrderPreviewLine{}, PaymentOptions: []billing.OrderPaymentOption{}}
	if len(cart.Lines) == 0 {
		return out, nil
	}
	quote, err := rt.Orders.Preview(ctx, customer.UUID(), cart.Lines)
	if err != nil {
		return nil, err
	}
	options, err := s.orderOptions(ctx, nil, quote.Prices(), quote.Products())
	if err != nil {
		return nil, err
	}
	preview := quote.Preview(options)
	for i := range preview.Lines {
		if preview.Lines[i].PriceID.IsZero() {
			preview.Lines[i].PriceID = cart.Lines[i].PriceID
		}
	}
	changed, expires := cart.LastChangedAt, cart.ExpiresAt()
	out.Currency, out.Total, out.Lines, out.PaymentOptions = preview.Currency, preview.Total, preview.Lines, preview.PaymentOptions
	out.LastChangedAt, out.ExpiresAt = &changed, &expires
	return out, nil
}
