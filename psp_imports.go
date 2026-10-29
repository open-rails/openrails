package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// CreatePSPImport pulls a PSP: OpenRails observes its catalog, pulls
// its customers, saved cards, live subscriptions and transaction history
// (reads only), links what it does not hold to customers by the import's
// rules, and proposes every difference in what it holds (ListProposals with
// Pull). One import per PSP pulls or applies at a time. The pull runs in the
// background; GetPSPImport reports it. ListProviderCatalog with Import then
// lists the plans to map.
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

// ListPSPImports lists the merchant's PSP imports, newest first: one PSP's
// when set.
func (c *Client) ListPSPImports(ctx context.Context, params billing.PSPImportListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.PSPImport], error) {
	q := pageValues(nil, params.PageRequest)
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	if !params.PSP.IsZero() {
		q.Set("psp", params.PSP.String())
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

// UpdatePSPImport replaces the import's rules; LinkPSPImport applies them.
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

// LinkPSPImport stores manual links and price picks, then relinks the import
// in the background: every staged record not yet imported is linked again by
// the import's rules and the merchant's mappings, and the ones OpenRails
// holds are marked held. The import is pulling until it has; GetPSPImport
// reports it.
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
// methods, subscriptions and payments, then approves its corrections (the
// proposals that keep or extend access) in the caller's name. Only a person
// signed in, or the in-process host, applies. It runs in the background. A
// subscription lands on the provider price mapping its plan; one whose plan
// is unmapped stays blocked until it is mapped (CreateProviderPrice), linked
// and applied again.
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

// ListProviderCatalog lists one PSP's own catalog (Stripe products and
// prices, NMI plans) as OpenRails last observed it, each price with the
// provider price mapping it. With Import it lists the plans that import's
// subscriptions name: map each by creating a provider price.
func (c *Client) ListProviderCatalog(ctx context.Context, params billing.ProviderCatalogListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.ProviderCatalogObject], error) {
	if params.PSP.IsZero() {
		return nil, invalidErr("psp is required")
	}
	q := pageValues(nil, params.PageRequest)
	q.Set("psp", params.PSP.String())
	if params.Kind != "" {
		q.Set("kind", string(params.Kind))
	}
	setBool(q, "mapped", params.Mapped)
	setBool(q, "retired", params.Retired)
	if params.Import != nil && !params.Import.IsZero() {
		q.Set("import", params.Import.String())
	}
	var out billing.ListPage[billing.ProviderCatalogObject]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/provider-catalog?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListParkedSubscriptions lists the live provider subscriptions OpenRails
// could not land, newest first: open parks, or closed ones with Released.
// Mapping a park's price or linking its customer lands it.
func (c *Client) ListParkedSubscriptions(ctx context.Context, params billing.ParkedSubscriptionListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.ParkedSubscription], error) {
	q := pageValues(nil, params.PageRequest)
	if params.PSP != "" {
		q.Set("psp", params.PSP)
	}
	if params.Reason != "" {
		q.Set("reason", string(params.Reason))
	}
	if params.Released {
		q.Set("released", "true")
	}
	var out billing.ListPage[billing.ParkedSubscription]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/parked-subscriptions?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
