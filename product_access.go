package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// ProductAccessClient checks access for a bounded set of products and lists
// purchases one page at a time. IDs use the same strings as the HTTP API.
type ProductAccessClient struct{ client *Client }

const ProductAccessMaxPageSize = 100

type ProductAccessCheckParams struct {
	CustomerID string
	ProductID  string
}
type ProductAccessCheckManyParams struct {
	CustomerID string
	ProductIDs []string
}
type ProductAccessListParams struct {
	CustomerID string
	Limit      int
	Cursor     string
}
type ProductAccessList struct {
	Data       []ProductAccessGrant `json:"data"`
	HasMore    bool                 `json:"has_more"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

func productAccessCustomerPath(id string) (string, error) {
	customer, err := ParseCustomerID(id)
	if err != nil || customer.IsZero() {
		return "", invalidErr("customer_id must be a nonzero UUID")
	}
	return "/v1/merchant/users/" + customer.String() + "/product-access", nil
}

func (s *ProductAccessClient) Check(ctx context.Context, params *ProductAccessCheckParams) (*ProductAccessCheck, error) {
	if params == nil {
		return nil, invalidErr("params are required")
	}
	path, err := productAccessCustomerPath(params.CustomerID)
	if err != nil {
		return nil, err
	}
	product, err := ParseProductID(params.ProductID)
	if err != nil || product.IsZero() {
		return nil, invalidErr("product_id is invalid")
	}
	var out ProductAccessCheck
	if err = s.client.do(ctx, http.MethodGet, path+"?product_id="+url.QueryEscape(product.String()), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CheckMany returns an explicit decision for each requested product. Empty input
// returns an empty map; duplicates share one decision. At most 100 IDs are accepted.
func (s *ProductAccessClient) CheckMany(ctx context.Context, params *ProductAccessCheckManyParams) (map[string]bool, error) {
	if params == nil {
		return nil, invalidErr("params are required")
	}
	path, err := productAccessCustomerPath(params.CustomerID)
	if err != nil {
		return nil, err
	}
	if len(params.ProductIDs) > ProductAccessMaxPageSize {
		return nil, invalidErr("at most 100 product IDs are allowed")
	}
	ids := make([]string, 0, len(params.ProductIDs))
	for _, id := range params.ProductIDs {
		product, err := ParseProductID(id)
		if err != nil || product.IsZero() {
			return nil, invalidErr("product_id is invalid")
		}
		ids = append(ids, product.String())
	}
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	var out struct {
		Access map[string]bool `json:"access"`
	}
	if err = s.client.do(ctx, http.MethodPost, path+"/check", struct {
		ProductIDs []string `json:"product_ids"`
	}{ids}, &out); err != nil {
		return nil, err
	}
	return out.Access, nil
}

func (s *ProductAccessClient) List(ctx context.Context, params *ProductAccessListParams) (*ProductAccessList, error) {
	if params == nil {
		return nil, invalidErr("params are required")
	}
	path, err := productAccessCustomerPath(params.CustomerID)
	if err != nil {
		return nil, err
	}
	if params.Limit < 0 || params.Limit > ProductAccessMaxPageSize {
		return nil, invalidErr("limit must be between 1 and 100")
	}
	query := url.Values{}
	if params.Limit > 0 {
		query.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		query.Set("cursor", params.Cursor)
	}
	var out ProductAccessList
	if err = s.client.do(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
