package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// StripeCollectionAdapter collects invoices through Stripe Invoicing using a
// saved Stripe payment method and merchant-scoped Stripe customer mapping.
type StripeCollectionAdapter struct {
	DB      *db.DB
	Service *subscriptions.StripeService
}

func NewStripeCollectionAdapter(database *db.DB, service *subscriptions.StripeService) *StripeCollectionAdapter {
	return &StripeCollectionAdapter{DB: database, Service: service}
}

func (a *StripeCollectionAdapter) ChargeSavedMethod(ctx context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (ChargeResult, error) {
	if a == nil || a.DB == nil || a.Service == nil {
		return ChargeResult{}, fmt.Errorf("stripe collection adapter not initialized")
	}
	paymentMethodID := strings.TrimSpace(method.VaultID)
	if paymentMethodID == "" || strings.HasPrefix(paymentMethodID, "stripe:") {
		return ChargeResult{}, fmt.Errorf("stripe payment method missing reusable payment_method id")
	}
	customerID, err := a.DB.Gen(ctx).GetProcessorCustomerIDForMerchant(ctx, gen.GetProcessorCustomerIDForMerchantParams{
		MerchantID: method.MerchantID,
		CustomerID: method.CustomerID,
		Processor:  string(models.ProcessorStripe),
	})
	if err != nil {
		return ChargeResult{}, fmt.Errorf("load stripe customer mapping: %w", err)
	}

	openRailsInvoiceID := ""
	if req.InvoiceID != nil {
		openRailsInvoiceID = req.InvoiceID.String()
	}
	result, err := a.Service.CollectInvoice(ctx, subscriptions.StripeInvoiceCollectionParams{
		CustomerID:          customerID,
		PaymentMethodID:     paymentMethodID,
		AmountCents:         req.AmountCents,
		Currency:            req.Currency,
		Description:         req.Description,
		IdempotencyKey:      req.IdempotencyKey,
		OpenRailsInvoiceID:  openRailsInvoiceID,
		OpenRailsMerchantID: method.MerchantID.String(),
		OpenRailsCustomerID: method.CustomerID.String(),
	})
	if err != nil {
		var stripeErr *subscriptions.StripeAPIError
		if errors.As(err, &stripeErr) && stripeErr.IsHardDecline() {
			code := stripeErr.FailureCode()
			message := stripeErr.Error()
			return ChargeResult{
				Processor:      string(models.ProcessorStripe),
				Declined:       true,
				FailureCode:    &code,
				FailureMessage: &message,
			}, nil
		}
		return ChargeResult{}, err
	}
	transactionID := strings.TrimSpace(result.ChargeID)
	if transactionID == "" {
		transactionID = strings.TrimSpace(result.PaymentIntentID)
	}
	if transactionID == "" {
		transactionID = strings.TrimSpace(result.InvoiceID)
	}
	return ChargeResult{
		Processor:         string(models.ProcessorStripe),
		TransactionID:     transactionID,
		ExternalInvoiceID: strings.TrimSpace(result.InvoiceID),
	}, nil
}
