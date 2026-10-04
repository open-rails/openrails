package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/open-rails/openrails/billing"
)

// ForCatalogOwner returns a catalog-only view of this client for one owner.
// The selector grants no authority: the server verifies the original credential,
// requiring catalog-administrator permission to select a different subject.
// The returned client cannot switch owner or call merchant-administrator APIs.
// Embedded and remote clients use the same routes and authorization checks.
func (c *Client) ForCatalogOwner(subject string) (*Client, error) {
	if subject == "" || !utf8.ValidString(subject) || strings.ContainsRune(subject, 0) {
		return nil, invalidErr("catalog owner subject must be nonempty UTF-8 text without NUL")
	}
	if c == nil {
		return nil, invalidErr("client is required")
	}
	if c.catalogOwner != "" && c.catalogOwner != subject {
		return nil, &billing.StatusError{Status: http.StatusForbidden, ErrorDetails: billing.ErrorDetails{Type: "invalid_request_error", Code: billing.CodeResourceAccessDenied, Message: "catalog-scoped clients cannot change owner"}}
	}
	scoped := *c
	scoped.derived = true
	scoped.ownCatalog = true
	scoped.catalogOwner = subject
	scoped.initResources()
	return &scoped, nil
}

// EnsureOwnCatalog returns the catalog belonging to the Gate-verified subject.
// Use ForCatalogOwner for an explicitly scoped host client, or WithOwnCatalog
// with a credential whose Gate resolves the owner subject and permissions.
func (c *Client) EnsureOwnCatalog(ctx context.Context, requestOptions ...RequestOption) (*billing.Catalog, error) {
	var out billing.Catalog
	if err := c.do(ctx, http.MethodPut, "/v1/catalog", struct{}{}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// EnsureCatalogForOwner is a merchant-administrator operation. The supplied
// subject selects business data; the credential's merchant grant authorizes it.
func (c *Client) EnsureCatalogForOwner(ctx context.Context, subject string, requestOptions ...RequestOption) (*billing.Catalog, error) {
	if subject == "" || !utf8.ValidString(subject) || strings.ContainsRune(subject, 0) {
		return nil, invalidErr("catalog owner subject must be nonempty UTF-8 text without NUL")
	}
	var out billing.Catalog
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/catalogs", struct {
		OwnerSubject string `json:"owner_subject"`
	}{OwnerSubject: subject}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCatalog reads one catalog by ID with merchant-administrator authority.
func (c *Client) GetCatalog(ctx context.Context, id billing.CatalogID, requestOptions ...RequestOption) (*billing.Catalog, error) {
	key, err := requireTypedID("catalog_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Catalog
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalogs/"+key, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCatalogForOwner retrieves an existing catalog using merchant-administrator
// authority. It returns ErrNotFound without creating anything when absent.
func (c *Client) GetCatalogForOwner(ctx context.Context, subject string, requestOptions ...RequestOption) (*billing.Catalog, error) {
	if subject == "" || !utf8.ValidString(subject) || strings.ContainsRune(subject, 0) {
		return nil, invalidErr("catalog owner subject must be nonempty UTF-8 text without NUL")
	}
	var out billing.Catalog
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalogs/by-owner?owner_subject="+url.QueryEscape(subject), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCatalogs returns one page of catalogs visible to a merchant administrator.
func (c *Client) ListCatalogs(ctx context.Context, options billing.PageOptions, requestOptions ...RequestOption) ([]billing.Catalog, error) {
	var out struct {
		Items []billing.Catalog `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalogs?"+pageQuery(options).Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
}
