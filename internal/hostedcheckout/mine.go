package hostedcheckout

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// AttachMine gives a customer's own open order its embedded checkout, in
// place of the one it had (the last secret stops working), and answers it with
// its URL on origin. It lasts as long as the order is open, never sliding its
// expiry, offers the customer's saved cards and returns nowhere: the frame
// tells its page, and redirect PSPs come back inside it. ok is false when the
// order is not the customer's open order.
func AttachMine(ctx context.Context, q *gen.Queries, origin string, mid billing.MerchantID, customer billing.CustomerID, order billing.OrderID, orderExpiry *time.Time, now time.Time) (billing.OrderCheckout, bool, error) {
	if origin == "" {
		return billing.OrderCheckout{}, false, apperr.New(http.StatusBadRequest, "hosted_checkout_unavailable", "this deployment serves no hosted checkout")
	}
	if orderExpiry == nil {
		return billing.OrderCheckout{}, false, errors.New("hostedcheckout: a customer's order expires")
	}
	secret, hash, err := NewSecret()
	if err != nil {
		return billing.OrderCheckout{}, false, err
	}
	n, err := q.SetMyOrderCheckout(ctx, gen.SetMyOrderCheckoutParams{
		SecretHash: hash, Now: now, MerchantID: mid.UUID(), ID: order.UUID(), CustomerID: customer.UUID(),
	})
	if err != nil || n != 1 {
		return billing.OrderCheckout{}, false, err
	}
	url := URL(origin, order, secret)
	out := Checkout{OrderID: order, CustomerID: customer, UIMode: billing.CheckoutEmbedded, ExpiresAt: orderExpiry.UTC(), SavedPaymentMethods: true}.View()
	out.URL = &url
	return out, true, nil
}
