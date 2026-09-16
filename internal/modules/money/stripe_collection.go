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
	customerID, err := a.stripeCustomerID(ctx, method)
	if err != nil {
		return nil, err
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
			if refused, ok := stripeDefinitiveRefusal(err); ok {
				return refused, nil
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
			Rail:              string(models.RailStripe),
			TransactionID:     transactionID,
			ExternalInvoiceID: strings.TrimSpace(result.InvoiceID),
		}, nil
	}), nil
}

func (a *StripeCollectionAdapter) stripeCustomerID(ctx context.Context, method gen.OpenrailsPaymentMethod) (string, error) {
	customerID, err := a.DB.Gen(ctx).GetRailCustomerAccountIDForMerchant(ctx, gen.GetRailCustomerAccountIDForMerchantParams{
		MerchantID: method.MerchantID,
		CustomerID: method.CustomerID,
		Rail:       string(models.RailStripe),
	})
	if err != nil {
		return "", fmt.Errorf("load stripe customer mapping: %w", err)
	}
	return customerID, nil
}

// stripeDefinitiveRefusal classifies a Stripe error answer. A 4xx other than
// an idempotency conflict or rate limit is a parsed refusal: Stripe processed
// the request and rejected it, so no money moved. Everything else (transport
// loss, 409/429, 5xx, an invoice left unpaid after /pay) is a possible
// submission that only an idempotent replay or a receipt can settle.
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

// stripeReceiptMatches requires the exact invoice to be paid for the frozen
// operation identity and amount.
func stripeReceiptMatches(r subscriptions.StripeCollectionReceipt, key string, amount moneyutil.Cents, currency string) error {
	switch {
	case r.CollectionKey != strings.TrimSpace(key):
		return fmt.Errorf("stripe invoice %s does not carry this operation's collection key", r.InvoiceID)
	case !strings.EqualFold(r.Status, "paid"):
		return fmt.Errorf("stripe invoice %s is %s, not paid", r.InvoiceID, r.Status)
	case !strings.EqualFold(r.Currency, currency):
		return fmt.Errorf("stripe invoice %s is in %s, not %s", r.InvoiceID, r.Currency, currency)
	case moneyutil.Cents(r.AmountPaid) < amount:
		return fmt.Errorf("stripe invoice %s paid %d of %d", r.InvoiceID, r.AmountPaid, amount)
	}
	return nil
}

func stripeReceiptTransactionID(r subscriptions.StripeCollectionReceipt) string {
	for _, id := range []string{r.ChargeID, r.PaymentIntentID, r.InvoiceID} {
		if id = strings.TrimSpace(id); id != "" {
			return id
		}
	}
	return ""
}
