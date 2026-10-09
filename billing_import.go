package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// ImportBilling lands declared billing facts under the bound merchant.
func (c *Client) ImportBilling(ctx context.Context, book billing.DeclaredBilling, requestOptions ...RequestOption) (*billing.BillingImportResult, error) {
	if book.AsOf.IsZero() {
		return nil, invalidErr("as_of is required")
	}
	var out billing.BillingImportResult
	if err := c.do(ctx, http.MethodPost, "/v1/admin/billing-import", book, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
