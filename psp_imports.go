package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// CreatePSPImport starts importing a PSP's subscribers: OpenRails pulls its
// customers, saved cards, live subscriptions and transaction history (reads
// only) and links each to a customer by the import's rules. The pull runs in
// the background; GetPSPImport reports it.
func (c *Client) CreatePSPImport(ctx context.Context, params billing.CreatePSPImportParams, requestOptions ...RequestOption) (*billing.PSPImport, error) {
	if params.PSP.IsZero() {
		return nil, invalidErr("psp is required")
	}
	var out billing.PSPImport
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/psp-imports", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPSPImports lists the merchant's PSP imports, newest first.
func (c *Client) ListPSPImports(ctx context.Context, params billing.PSPImportListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.PSPImport], error) {
	q := pageValues(nil, params.PageRequest)
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.PSPImport]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/psp-imports?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPSPImport reads one PSP import with its records counted.
func (c *Client) GetPSPImport(ctx context.Context, id billing.PSPImportID, requestOptions ...RequestOption) (*billing.PSPImport, error) {
	path, err := requireTypedID("psp_import_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.PSPImport
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/psp-imports/"+path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdatePSPImport replaces the import's rules or products; LinkPSPImport
// applies new rules.
func (c *Client) UpdatePSPImport(ctx context.Context, id billing.PSPImportID, params billing.UpdatePSPImportParams, requestOptions ...RequestOption) (*billing.PSPImport, error) {
	path, err := requireTypedID("psp_import_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.PSPImport
	if err := c.do(ctx, http.MethodPatch, "/v1/merchant/psp-imports/"+path, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeletePSPImport deletes an import and its staged records; what it landed
// stays.
func (c *Client) DeletePSPImport(ctx context.Context, id billing.PSPImportID, requestOptions ...RequestOption) error {
	path, err := requireTypedID("psp_import_id", id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, "/v1/merchant/psp-imports/"+path, nil, nil, requestOptions...)
}

// ListPSPImportRecords lists an import's staged provider objects, by state
// or kind: the unmatched ones are what no rule linked.
func (c *Client) ListPSPImportRecords(ctx context.Context, id billing.PSPImportID, params billing.PSPImportRecordListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.PSPImportRecord], error) {
	path, err := requireTypedID("psp_import_id", id)
	if err != nil {
		return nil, err
	}
	q := pageValues(nil, params.PageRequest)
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	if params.State != "" {
		q.Set("state", string(params.State))
	}
	if params.Kind != "" {
		q.Set("kind", string(params.Kind))
	}
	var out billing.ListPage[billing.PSPImportRecord]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/psp-imports/"+path+"/records?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// LinkPSPImport stores manual links and links every staged record again by
// the import's rules and the merchant's mappings.
func (c *Client) LinkPSPImport(ctx context.Context, id billing.PSPImportID, params billing.LinkPSPImportParams, requestOptions ...RequestOption) (*billing.PSPImport, error) {
	path, err := requireTypedID("psp_import_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.PSPImport
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/psp-imports/"+path+"/link", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ApplyPSPImport lands the import's linked records as customers, payment
// methods, subscriptions and payments. It runs in the background.
func (c *Client) ApplyPSPImport(ctx context.Context, id billing.PSPImportID, requestOptions ...RequestOption) (*billing.PSPImport, error) {
	path, err := requireTypedID("psp_import_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.PSPImport
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/psp-imports/"+path+"/apply", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetPSPImportMappings maps the merchant's own identifiers (a legacy user or
// order id) to customers, for import rules to read.
func (c *Client) SetPSPImportMappings(ctx context.Context, params billing.SetPSPImportMappingsParams, requestOptions ...RequestOption) (*billing.PSPImportMappingsResult, error) {
	if len(params.Mappings) == 0 || len(params.Mappings) > billing.MaxBatchItems {
		return nil, invalidErr("mappings holds 1 to 100 items")
	}
	var out billing.PSPImportMappingsResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/psp-import-mappings", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
