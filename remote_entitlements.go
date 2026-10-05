package openrails

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/open-rails/openrails/billing"
)

// CreateEntitlement grants the customer an entitlement as the merchant's own
// grant.
func (c *Client) CreateEntitlement(ctx context.Context, customerID billing.CustomerID, params billing.CreateEntitlementParams, requestOptions ...RequestOption) (*billing.EntitlementRecord, error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	if params.Entitlement = strings.TrimSpace(params.Entitlement); params.Entitlement == "" {
		return nil, invalidErr("entitlement is required")
	}
	var out billing.EntitlementRecord
	if err := c.do(ctx, http.MethodPost, path+"/entitlements", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteEntitlement revokes one of the customer's entitlement windows.
func (c *Client) DeleteEntitlement(ctx context.Context, customerID billing.CustomerID, id billing.EntitlementID, requestOptions ...RequestOption) error {
	path, err := customerIDPath(customerID)
	if err != nil {
		return err
	}
	entitlement, err := requireTypedID("entitlement_id", id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path+"/entitlements/"+entitlement, nil, nil, requestOptions...)
}

// CheckProductAccess reports, for each product named by exactly one of
// params.ProductIDs and params.ProductKeys, whether the customer has access to
// it now. Keys of the result are the ids or keys the request named.
func (c *Client) CheckProductAccess(ctx context.Context, customerID billing.CustomerID, params billing.CheckProductAccessParams, requestOptions ...RequestOption) (map[string]bool, error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	if (params.ProductIDs == nil) == (params.ProductKeys == nil) {
		return nil, invalidErr("exactly one of product_ids and product_keys is required")
	}
	for _, key := range params.ProductKeys {
		if !validProductKey(key) {
			return nil, invalidErr("product_key is invalid")
		}
	}
	var out billing.ProductAccessCheck
	if err := c.do(ctx, http.MethodPost, path+"/product-access/check", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Access, nil
}

// ListProductAccess returns one page of the products the customer has access
// to.
func (c *Client) ListProductAccess(ctx context.Context, customerID billing.CustomerID, page billing.PageRequest, requestOptions ...RequestOption) (*billing.ListPage[billing.ProductAccessGrant], error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.ProductAccessGrant]
	if err := c.do(ctx, http.MethodGet, path+"/product-access?"+pageValues(nil, page).Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateProductAccess grants the customer access to a product.
func (c *Client) CreateProductAccess(ctx context.Context, customerID billing.CustomerID, params billing.CreateProductAccessParams, requestOptions ...RequestOption) (*billing.ProductAccessGrant, error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	if params.ProductID.IsZero() {
		return nil, invalidErr("product_id is required")
	}
	var out billing.ProductAccessGrant
	if err := c.do(ctx, http.MethodPost, path+"/product-access", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteProductAccess revokes one of the customer's product-access grants.
func (c *Client) DeleteProductAccess(ctx context.Context, customerID billing.CustomerID, id billing.ProductAccessID, requestOptions ...RequestOption) error {
	path, err := customerIDPath(customerID)
	if err != nil {
		return err
	}
	grant, err := requireTypedID("product_access_id", id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path+"/product-access/"+grant, nil, nil, requestOptions...)
}

func validProductKey(key string) bool {
	return strings.TrimSpace(key) != "" && utf8.ValidString(key) && !strings.ContainsRune(key, 0)
}
