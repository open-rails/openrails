package openrails

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// HasSettledPayment reports whether this merchant's customer has ever completed
// a positive rail payment for the given price. Refunding that payment does not
// erase the historical fact. This reads the authoritative payment record and
// remains true after its acknowledged host event is pruned.
func (c *Client) HasSettledPayment(ctx context.Context, customerID CustomerID, priceID uuid.UUID) (bool, error) {
	var out struct {
		Settled bool `json:"settled"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/merchant/customers/"+customerID.String()+"/payment-settlement-status?price_id="+priceID.String(), nil, &out)
	return out.Settled, err
}
