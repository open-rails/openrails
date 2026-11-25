package service

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// The customer's cart: lines kept across the merchant's sites, priced live as
// an invoice preview, and bought with cart: true.

// checkInvoiceLines refuses a request naming both or neither of lines and the
// cart.
func checkInvoiceLines(lines []billing.InvoiceLineParams, cart bool) error {
	switch {
	case cart && len(lines) > 0:
		return apperr.Invalidf("send lines or cart: true, not both").WithParam("cart")
	case cart:
		return nil
	}
	return invoices.CheckLines(lines)
}

// invoiceLinesOf is what an invoice request buys: its lines, or the customer's
// cart.
func invoiceLinesOf(ctx context.Context, rt *invoiceRuntime, customer billing.CustomerID, lines []billing.InvoiceLineParams, cart bool) ([]billing.InvoiceLineParams, error) {
	if err := checkInvoiceLines(lines, cart); err != nil || !cart {
		return lines, err
	}
	c, err := rt.Invoices.Cart(ctx, customer.UUID())
	if err != nil {
		return nil, err
	}
	if len(c.Lines) == 0 {
		return nil, apperr.Invalidf("the cart is empty").WithParam("cart")
	}
	return c.Lines, nil
}

// GetCart reads the customer's cart, priced now.
func (s *Service) GetCart(ctx context.Context, actor InvoiceActor) (*billing.Cart, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return nil, err
	}
	return s.cartView(ctx, rt, actor)
}

// SetCartLine puts a price in the customer's cart, changes its line, or
// (quantity 0) removes it, and answers the cart.
func (s *Service) SetCartLine(ctx context.Context, actor InvoiceActor, price billing.PriceID, params billing.SetCartLineParams) (*billing.Cart, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return nil, err
	}
	resolved, err := s.rt.PriceService.GetByPublicID(ctx, price)
	if err != nil {
		return nil, priceLookup(err)
	}
	if err := rt.Invoices.SetCartLine(ctx, actor.Customer.UUID(), resolved.ID, params.Quantity, params.Amount); err != nil {
		return nil, err
	}
	return s.cartView(ctx, rt, actor)
}

// ClearCart empties the customer's cart.
func (s *Service) ClearCart(ctx context.Context, actor InvoiceActor) error {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return err
	}
	return rt.Invoices.ClearCart(ctx, actor.Customer.UUID())
}

// cartView prices the customer's cart as an invoice preview; a line whose
// price is gone keeps its price id, refused.
func (s *Service) cartView(ctx context.Context, rt *invoiceRuntime, actor InvoiceActor) (*billing.Cart, error) {
	customer := actor.Customer
	cart, err := rt.Invoices.Cart(ctx, customer.UUID())
	if err != nil {
		return nil, err
	}
	out := &billing.Cart{Lines: []billing.InvoicePreviewLine{}, PaymentOptions: []billing.InvoicePaymentOption{}}
	if len(cart.Lines) == 0 {
		return out, nil
	}
	quote, err := rt.Invoices.Preview(ctx, customer.UUID(), cart.Lines)
	if err != nil {
		return nil, err
	}
	options, err := s.invoiceOptions(ctx, customer.UUID(), &actor, quote.Prices(), quote.Products())
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
