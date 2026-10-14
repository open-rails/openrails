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

// AttachMine gives a customer's own open order its embedded pay URL, in
// place of the one it had (the last secret stops working), and answers it with
// its URL on origin. It lasts as long as the order is open, never sliding its
// expiry, offers the customer's saved cards and returns nowhere: the frame
// tells its page, and redirect PSPs come back inside it. ok is false when the
// order is not the customer's open order.
func AttachMine(ctx context.Context, q *gen.Queries, origin string, mid billing.MerchantID, customer billing.CustomerID, order billing.OrderID, orderExpiry *time.Time, now time.Time) (PayURL, string, bool, error) {
	if origin == "" {
		return PayURL{}, "", false, apperr.New(http.StatusBadRequest, "pay_url_unavailable", "this deployment serves no checkout origin")
	}
	if orderExpiry == nil {
		return PayURL{}, "", false, errors.New("paymentpage: a customer's order expires")
	}
	secret, hash, err := NewSecret()
	if err != nil {
		return PayURL{}, "", false, err
	}
	n, err := q.SetMyOrderPayURL(ctx, gen.SetMyOrderPayURLParams{
		SecretHash: hash, Now: now, MerchantID: mid.UUID(), ID: order.UUID(), CustomerID: customer.UUID(),
	})
	if err != nil || n != 1 {
		return PayURL{}, "", false, err
	}
	return PayURL{MerchantID: mid, OrderID: order, CustomerID: customer, Embedded: true, ExpiresAt: orderExpiry.UTC(), SavedPaymentMethods: true},
		URL(origin, order, secret), true, nil
}
