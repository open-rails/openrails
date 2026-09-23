package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ProductAccessClient checks access for a bounded set of products and lists
// purchases one page at a time. IDs use the same strings as the HTTP API.
type ProductAccessClient struct{ client *Client }

const ProductAccessMaxPageSize = 100

type ProductAccessCheckParams struct {
	CustomerID string
	ProductID  string
	ProductKey string
}
type ProductAccessCheckManyParams struct {
	CustomerID  string
	ProductIDs  []string
	ProductKeys []string
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

func (s *ProductAccessClient) Check(ctx context.Context, params *ProductAccessCheckParams, requestOptions ...RequestOption) (*ProductAccessCheck, error) {
	if params == nil {
		return nil, invalidErr("params are required")
	}
	path, err := productAccessCustomerPath(params.CustomerID)
	if err != nil {
		return nil, err
	}
	if (params.ProductID == "") == (params.ProductKey == "") {
		return nil, invalidErr("exactly one of product_id and product_key is required")
	}
	query := url.Values{}
	if params.ProductKey != "" {
		if !validProductKey(params.ProductKey) {
			return nil, invalidErr("product_key is invalid")
		}
		query.Set("product_key", params.ProductKey)
	} else {
		product, err := ParseProductID(params.ProductID)
		if err != nil || product.IsZero() {
			return nil, invalidErr("product_id is invalid")
		}
		query.Set("product_id", product.String())
	}
	var out ProductAccessCheck
	if err = s.client.do(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CheckMany returns an explicit decision for each requested product. Empty input
// returns an empty map; duplicates share one decision. Supply exactly one nonnil
// ProductIDs or ProductKeys collection, at most 100 entries. Result keys match
// the supplied coordinates; missing products have an explicit false decision.
func (s *ProductAccessClient) CheckMany(ctx context.Context, params *ProductAccessCheckManyParams, requestOptions ...RequestOption) (map[string]bool, error) {
	if params == nil {
		return nil, invalidErr("params are required")
	}
	path, err := productAccessCustomerPath(params.CustomerID)
	if err != nil {
		return nil, err
	}
	if (params.ProductIDs == nil) == (params.ProductKeys == nil) {
		return nil, invalidErr("exactly one of product_ids and product_keys is required")
	}
	if len(params.ProductIDs)+len(params.ProductKeys) > ProductAccessMaxPageSize {
		return nil, invalidErr("at most 100 products are allowed")
	}
	for _, key := range params.ProductKeys {
		if !validProductKey(key) {
			return nil, invalidErr("product_key is invalid")
		}
	}
	var ids []string
	if params.ProductIDs != nil {
		ids = make([]string, 0, len(params.ProductIDs))
	}
	for _, id := range params.ProductIDs {
		product, err := ParseProductID(id)
		if err != nil || product.IsZero() {
			return nil, invalidErr("product_id is invalid")
		}
		ids = append(ids, product.String())
	}
	if len(ids)+len(params.ProductKeys) == 0 {
		return map[string]bool{}, nil
	}
	var out struct {
		Access map[string]bool `json:"access"`
	}
	if err = s.client.do(ctx, http.MethodPost, path+"/check", struct {
		ProductIDs  []string `json:"product_ids"`
		ProductKeys []string `json:"product_keys"`
	}{ids, params.ProductKeys}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Access, nil
}

func validProductKey(key string) bool {
	return strings.TrimSpace(key) != "" && utf8.ValidString(key) && !strings.ContainsRune(key, 0)
}

func (s *ProductAccessClient) List(ctx context.Context, params *ProductAccessListParams, requestOptions ...RequestOption) (*ProductAccessList, error) {
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
	if err = s.client.do(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
