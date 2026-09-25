package money

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// StripeCollectionAdapter collects invoices through Stripe Invoicing using a
// saved Stripe payment method and merchant-scoped Stripe customer mapping.
// Every request in the sequence carries an idempotency key derived from the
// operation identity, so a replay of the same operation returns the original
// objects instead of minting a second charge (within Stripe's key window).
type StripeCollectionAdapter struct {
	DB      *db.DB
	Service *subscriptions.StripeService
}

func NewStripeCollectionAdapter(database *db.DB, service *subscriptions.StripeService) *StripeCollectionAdapter {
	return &StripeCollectionAdapter{DB: database, Service: service}
}

func (a *StripeCollectionAdapter) Prepare(ctx context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (PreparedCharge, error) {
	if a == nil || a.DB == nil || a.Service == nil {
		return nil, fmt.Errorf("stripe collection adapter not initialized")
	}
	paymentMethodID := strings.TrimSpace(method.RailMethodRef)
	if paymentMethodID == "" || strings.HasPrefix(paymentMethodID, "stripe:") {
		return nil, fmt.Errorf("stripe payment method missing reusable payment_method id")
	}
	if err := moneyutil.ValidateCurrency(req.Currency); err != nil {
		return nil, fmt.Errorf("stripe collection: refusing to charge without an established currency: %w", err)
	}
	customerID := req.ProviderCustomerRef
	if customerID == "" || req.Instrument.RailMethodRef != paymentMethodID {
		return nil, errors.New("Stripe collection requires the frozen provider customer and method")
	}
	openRailsInvoiceID := ""
	if req.InvoiceID != nil {
		openRailsInvoiceID = req.InvoiceID.String()
	}
	params := subscriptions.StripeInvoiceCollectionParams{
		CustomerID:          customerID,
		PaymentMethodID:     paymentMethodID,
		AmountCents:         req.AmountCents,
		Currency:            req.Currency,
		Description:         req.Description,
		IdempotencyKey:      strings.TrimSpace(req.IdempotencyKey),
		OpenRailsInvoiceID:  openRailsInvoiceID,
		OpenRailsMerchantID: method.MerchantID.String(),
		OpenRailsCustomerID: method.CustomerID.String(),
	}
	return PreparedChargeFunc(func(ctx context.Context) (ChargeResult, error) {
		result, err := a.Service.CollectInvoice(ctx, params)
		if err != nil {
			refused, ok := stripeDefinitiveRefusal(err)
			if !ok {
				return ChargeResult{}, err
			}
			// A refusal is definitive only once nothing of this operation can
			// still be charged at Stripe: the sequence may have left a draft or
			// open invoice behind. Cleanup failure keeps the outcome unknown.
			if cerr := a.Service.CleanupCollection(ctx, customerID, params.IdempotencyKey); cerr != nil {
				return ChargeResult{}, fmt.Errorf("stripe refused (%v) but its objects could not be cleaned up: %w", err, cerr)
			}
			return refused, nil
		}
		transactionID := strings.TrimSpace(result.ChargeID)
		if transactionID == "" {
			transactionID = strings.TrimSpace(result.PaymentIntentID)
		}
		return ChargeResult{
			Rail:              string(models.RailStripe),
			TransactionID:     transactionID,
			ExternalInvoiceID: strings.TrimSpace(result.InvoiceID),
		}, nil
	}), nil
}

// stripeDefinitiveRefusal classifies a Stripe error answer. A 4xx other than
// an idempotency conflict or rate limit is a parsed refusal: Stripe processed
// the request and rejected it, so no money moved by that request. Everything
// else (transport loss, 409/429, 5xx, an invoice left unpaid after /pay) is a
// possible submission that only an idempotent replay or a receipt can settle.
func stripeDefinitiveRefusal(err error) (ChargeResult, bool) {
	var apiErr *subscriptions.StripeAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode < 400 || apiErr.StatusCode >= 500 {
		return ChargeResult{}, false
	}
	switch apiErr.StatusCode {
	case http.StatusConflict, http.StatusTooManyRequests:
		return ChargeResult{}, false
	}
	code := apiErr.FailureCode()
	message := apiErr.Error()
	return ChargeResult{Rail: string(models.RailStripe), Declined: true, FailureCode: &code, FailureMessage: &message}, true
}
