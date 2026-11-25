package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// GetInvoicePaymentAttempt reads the exact durable purchase attempt. A nil actor
// is the already-authorized merchant route; customer callers supply their
// original scoped actor. No provider, clock expiry or idempotency write runs.
func (s *Service) GetInvoicePaymentAttempt(ctx context.Context, actor *InvoiceActor, id billing.InvoiceID, key string) (*billing.InvoicePaymentAttempt, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var invoice *invoices.Invoice
	if actor == nil {
		invoice, err = rt.Invoices.Get(ctx, id.UUID())
	} else {
		invoice, err = rt.Invoices.GetForCustomer(ctx, actor.Customer.UUID(), id.UUID())
	}
	if err != nil {
		return nil, err
	}
	attemptID := payment.InvoiceAttemptID(mid.UUID(), invoice.ID, "pay:"+key)
	attempt, err := rt.DB.Gen(ctx).GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, invoices.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if attempt.CustomerID != invoice.CustomerID || attempt.InvoiceID == nil || *attempt.InvoiceID != invoice.ID {
		return nil, invoices.ErrNotFound
	}
	if attempt.Amount == nil || attempt.Currency == nil {
		return nil, fmt.Errorf("invoice attempt has no accepted amount")
	}
	result := &billing.InvoicePaymentAttempt{InvoiceID: id, CustomerID: billing.CustomerID(invoice.CustomerID), AttemptID: billing.InvoiceAttemptID(attempt.ID),
		State: attempt.Status, PSPID: billing.PSPID(attempt.PspID), Amount: *attempt.Amount, Currency: catalog.Currency(*attempt.Currency)}
	if attempt.PaymentID != nil {
		value := billing.PaymentID(*attempt.PaymentID)
		result.PaymentID = &value
	}
	// The same immutable operation key is used by purchase admission. Unlike
	// an invoice's latest payment, it cannot name a competing accepted attempt.
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
	if accepted.InvoiceID != invoice.ID || accepted.InvoiceAttemptID != attempt.ID || accepted.UserID != invoice.CustomerID.String() || accepted.Instrument.PSPID != attempt.PspID {
		return nil, fmt.Errorf("invoice payment receipt does not match its accepted operation")
	}
	if attempt.Status == "succeeded" && !accepted.PurchaseScoped && accepted.PaymentMethodID != uuid.Nil {
		value := billing.PaymentMethodID(accepted.PaymentMethodID)
		result.PaymentMethodID = &value
	}
	return result, nil
}
