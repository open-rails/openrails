package openrails

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// Customer is one merchant-scoped billing record. Its ID is the host's stable
// subject UUID; the same UUID under another merchant is a different customer.
type Customer struct {
	ID         CustomerID `json:"id"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
}

// EnsureCustomer materializes the customer record for id under the bound
// merchant, or refreshes last_seen_at when it already exists. Commerce writes
// materialize customers on demand; call this when a host must reference the
// customer before its first purchase.
func (c *Client) EnsureCustomer(ctx context.Context, id CustomerID) (*Customer, error) {
	if uuid.UUID(id) == uuid.Nil {
		return nil, invalidErr("customer id is required")
	}
	var out Customer
	path := "/v1/merchant/customers/" + url.PathEscape(uuid.UUID(id).String())
	if err := c.do(ctx, http.MethodPut, path, struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
