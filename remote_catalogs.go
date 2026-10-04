package openrails

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/open-rails/openrails/billing"
)

// ForCatalogOwner returns a client whose product, price and offer methods act
// on one creator's own catalog, created on its first write. The selector
// grants no authority: a credential whose verified subject is the owner may
// use it, any other needs catalog-administrator permission. The returned
// client cannot switch owner or call any other operation.
func (c *Client) ForCatalogOwner(subject string) (*Client, error) {
	if err := validOwnerSubject(subject); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, invalidErr("client is required")
	}
	if c.catalogOwner != "" && c.catalogOwner != subject {
		return nil, &billing.StatusError{Status: http.StatusForbidden, ErrorDetails: billing.ErrorDetails{Type: "invalid_request_error", Code: billing.CodeResourceAccessDenied, Message: "catalog-scoped clients cannot change owner"}}
	}
	scoped := *c
	scoped.derived = true
	scoped.catalogOwner = subject
	return &scoped, nil
}

func validOwnerSubject(subject string) error {
	if subject == "" || !utf8.ValidString(subject) || strings.ContainsRune(subject, 0) {
		return invalidErr("catalog owner subject must be nonempty UTF-8 text without NUL")
	}
	return nil
}

// EnsureCatalog returns a creator's catalog, creating it the first time.
func (c *Client) EnsureCatalog(ctx context.Context, params billing.EnsureCatalogParams, requestOptions ...RequestOption) (*billing.Catalog, error) {
	if err := validOwnerSubject(params.OwnerSubject); err != nil {
		return nil, err
	}
	var out billing.Catalog
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/catalogs", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCatalog reads one of the merchant's catalogs.
func (c *Client) GetCatalog(ctx context.Context, id billing.CatalogID, requestOptions ...RequestOption) (*billing.Catalog, error) {
	path, err := requireTypedID("catalog_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Catalog
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalogs/"+path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCatalogs returns one page of the merchant's catalogs, oldest first;
// params.OwnerSubject selects one creator's.
func (c *Client) ListCatalogs(ctx context.Context, params billing.CatalogListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Catalog], error) {
	q := pageValues(nil, params.PageRequest)
	if params.OwnerSubject != "" {
		q.Set("owner_subject", params.OwnerSubject)
	}
	var out billing.ListPage[billing.Catalog]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalogs?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
