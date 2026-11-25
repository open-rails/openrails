package paymentpage

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// AttachMine gives a customer's own open invoice its embedded pay URL, in
// place of the one it had (the last secret stops working), and answers it with
// its URL on origin. It lasts as long as the invoice is open, never sliding its
// expiry, offers the customer's saved cards and returns nowhere: the frame
// tells its page, and redirect PSPs come back inside it. ok is false when the
// invoice is not the customer's open invoice.
func AttachMine(ctx context.Context, q *gen.Queries, origin string, mid billing.MerchantID, customer billing.CustomerID, invoice billing.InvoiceID, invoiceExpiry *time.Time, now time.Time) (PayURL, string, bool, error) {
	if origin == "" {
		return PayURL{}, "", false, apperr.New(http.StatusBadRequest, billing.CodePayURLUnavailable, "this deployment serves no checkout origin")
	}
	if invoiceExpiry == nil {
		return PayURL{}, "", false, errors.New("paymentpage: a customer's invoice expires")
	}
	secret, hash, err := NewSecret()
	if err != nil {
		return PayURL{}, "", false, err
	}
	n, err := q.SetMyInvoicePayURL(ctx, gen.SetMyInvoicePayURLParams{
		SecretHash: hash, Now: now, MerchantID: mid.UUID(), ID: invoice.UUID(), CustomerID: customer.UUID(),
	})
	if err != nil || n != 1 {
		return PayURL{}, "", false, err
	}
	return PayURL{MerchantID: mid, InvoiceID: invoice, CustomerID: customer, Embedded: true, ExpiresAt: invoiceExpiry.UTC(), SavedPaymentMethods: true},
		URL(origin, invoice, secret), true, nil
}
