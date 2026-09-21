package openrails

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"
)

// EnsureOwnCatalog returns the catalog belonging to the Gate-verified subject.
// No request owner ID is accepted; use Runtime.CatalogClient or WithOwnCatalog
// with a credential whose Gate resolves the owner subject and permissions.
func (c *Client) EnsureOwnCatalog(ctx context.Context) (*Catalog, error) {
	var out Catalog
	if err := c.do(ctx, http.MethodPut, "/v1/catalog", struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EnsureCatalogForOwner is a merchant-administrator operation. The supplied
// subject selects business data; the credential's merchant grant authorizes it.
func (c *Client) EnsureCatalogForOwner(ctx context.Context, subject string) (*Catalog, error) {
	if subject == "" || !utf8.ValidString(subject) || strings.ContainsRune(subject, 0) {
		return nil, invalidErr("catalog owner subject must be nonempty UTF-8 text without NUL")
	}
	var out Catalog
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/catalogs", struct {
		OwnerSubject string `json:"owner_subject"`
	}{OwnerSubject: subject}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetCatalog(ctx context.Context, id CatalogID) (*Catalog, error) {
	key, err := requireTypedID("catalog_id", id)
	if err != nil {
		return nil, err
	}
	var out Catalog
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalogs/"+key, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCatalogs returns one page of catalogs visible to a merchant administrator.
func (c *Client) ListCatalogs(ctx context.Context, options PageOptions) ([]Catalog, error) {
	var out struct {
		Items []Catalog `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalogs?"+pageQuery(options).Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
