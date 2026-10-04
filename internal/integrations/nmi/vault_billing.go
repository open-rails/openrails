package nmi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// ReadSingleCardVaultBilling proves the exact sole billing entry under this
// account. A nonempty billing identifier can be a normally-created native card.
func (c *NMIClient) ReadSingleCardVaultBilling(ctx context.Context, id, billingID string) (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("vault ID required")
	}
	var customer V5Customer
	if err := c.sendV5Request(ctx, http.MethodGet, "/customers/"+url.PathEscape(id), nil, &customer); err != nil {
		return "", err
	}
	if customer.Object != "customer" || customer.ID != id || len(customer.Billing) != 1 || customer.Billing[0].ID == "" || (billingID != "" && customer.Billing[0].ID != billingID) {
		return "", errors.New("a verified single-card vault is required")
	}
	return customer.Billing[0].ID, nil
}
