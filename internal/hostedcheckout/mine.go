package hostedcheckout

import (
	"context"
	"net/http"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// MyOrderLifetime is how long a customer's own checkout lasts.
const MyOrderLifetime = 15 * time.Minute

// AttachMine gives a customer's own open order its embedded checkout, in
// place of the one it had, and answers it with its URL on origin. It lasts
// MyOrderLifetime, or until the order expires when sooner, offers the
// customer's saved cards and returns to successURL. ok is false when the
// order is not the customer's open order.
func AttachMine(ctx context.Context, q *gen.Queries, origin string, mid billing.MerchantID, customer billing.CustomerID, order billing.OrderID, successURL string, orderExpiry *time.Time, now time.Time) (billing.OrderCheckout, bool, error) {
	if origin == "" {
		return billing.OrderCheckout{}, false, apperr.New(http.StatusBadRequest, "hosted_checkout_unavailable", "this deployment serves no hosted checkout")
	}
	now = now.UTC().Truncate(time.Microsecond)
	expires := now.Add(MyOrderLifetime)
	if orderExpiry != nil && orderExpiry.Before(expires) {
		expires = orderExpiry.UTC()
	}
	secret, hash, err := NewSecret()
	if err != nil {
		return billing.OrderCheckout{}, false, err
	}
	n, err := q.SetMyOrderCheckout(ctx, gen.SetMyOrderCheckoutParams{
		SecretHash: hash, ExpiresAt: expires, SuccessUrl: successURL, Now: now,
		MerchantID: mid.UUID(), ID: order.UUID(), CustomerID: customer.UUID(),
	})
	if err != nil || n != 1 {
		return billing.OrderCheckout{}, false, err
	}
	url := URL(origin, order, secret)
	out := Checkout{OrderID: order, CustomerID: customer, UIMode: billing.CheckoutEmbedded, ExpiresAt: expires, SuccessURL: successURL, SavedPaymentMethods: true}.View()
	out.URL = &url
	return out, true, nil
}
