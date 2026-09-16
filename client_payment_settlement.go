package openrails

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// HasSettledPayment reports whether this merchant's customer has ever completed
// a positive rail payment for the given price. Refunding that payment does not
// erase the historical fact, nor does archiving it. This reads the payment record and
// remains true after its acknowledged host event is pruned.
func (c *Client) HasSettledPayment(ctx context.Context, customerID CustomerID, priceID uuid.UUID) (bool, error) {
	customer, err := requireUUID("customer_id", uuid.UUID(customerID))
	if err != nil {
		return false, err
	}
	price, err := requireUUID("price_id", priceID)
	if err != nil {
		return false, err
	}
	var out struct {
		Settled bool `json:"settled"`
	}
	err = c.do(ctx, http.MethodGet, "/v1/merchant/customers/"+customer+"/payment-settlement-status?price_id="+price, nil, &out)
	return out.Settled, err
}
