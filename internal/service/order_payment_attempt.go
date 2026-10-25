package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// GetOrderPaymentAttempt reads the exact durable purchase attempt. A nil actor
// is the already-authorized merchant route; customer callers supply their
// original scoped actor. No provider, clock expiry or idempotency write runs.
func (s *Service) GetOrderPaymentAttempt(ctx context.Context, actor *OrderActor, id billing.OrderID, key string) (*billing.OrderPaymentAttempt, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var order *orders.Order
	if actor == nil {
		order, err = rt.Orders.Get(ctx, id.UUID())
	} else {
		order, err = rt.Orders.GetForCustomer(ctx, actor.Customer.UUID(), id.UUID())
	}
	if err != nil {
		return nil, err
	}
	attemptID := payment.OrderAttemptID(mid.UUID(), order.ID, "pay:"+key)
	attempt, err := rt.DB.Gen(ctx).GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, orders.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if attempt.CustomerID != order.CustomerID || attempt.OrderID == nil || *attempt.OrderID != order.ID {
		return nil, orders.ErrNotFound
	}
	if attempt.Amount == nil || attempt.Currency == nil {
		return nil, fmt.Errorf("order attempt has no accepted amount")
	}
	result := &billing.OrderPaymentAttempt{OrderID: id, CustomerID: billing.CustomerID(order.CustomerID), AttemptID: billing.OrderAttemptID(attempt.ID),
		State: attempt.Status, PSPID: billing.PSPID(attempt.PspID), Amount: *attempt.Amount, Currency: *attempt.Currency}
	if attempt.PaymentID != nil {
		value := billing.PaymentID(*attempt.PaymentID)
		result.PaymentID = &value
	}
	// The same immutable operation key is used by purchase admission. Unlike
	// an order's latest payment, it cannot name a competing accepted attempt.
	operation, err := rt.DB.Gen(ctx).GetProviderIntentByIdempotencyKey(ctx, gen.GetProviderIntentByIdempotencyKeyParams{
		MerchantID: mid.UUID(), IdempotencyKey: payment.NMISaleIdempotencyKey("order:" + attempt.ID.String())})
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	accepted, err := payments.DecodeNMISalePayload(operation)
	if err != nil {
		return nil, err
	}
	if accepted.OrderID != order.ID || accepted.OrderAttemptID != attempt.ID || accepted.UserID != order.CustomerID.String() || accepted.Instrument.PSPID != attempt.PspID {
		return nil, fmt.Errorf("order payment receipt does not match its accepted operation")
	}
	if attempt.Status == "succeeded" && !accepted.PurchaseScoped && accepted.PaymentMethodID != uuid.Nil {
		value := billing.PaymentMethodID(accepted.PaymentMethodID)
		result.PaymentMethodID = &value
	}
	return result, nil
}
