package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/open-rails/openrails/billing"
)

// ListPSPs lists the merchant's PSPs, newest first. Credential values are
// never returned.
func (c *Client) ListPSPs(ctx context.Context, params billing.PSPListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.PSP], error) {
	query := url.Values{}
	if params.Rail != "" {
		query.Set("rail", string(params.Rail))
	}
	if params.Archived != nil {
		query.Set("archived", strconv.FormatBool(*params.Archived))
	}
	if params.Limit != 0 {
		query.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		query.Set("cursor", params.Cursor)
	}
	path := "/v1/merchant/psps"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var out billing.ListPage[billing.PSP]
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPSP reads one PSP.
func (c *Client) GetPSP(ctx context.Context, id billing.PSPID, requestOptions ...RequestOption) (*billing.PSP, error) {
	path, err := pspPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.PSP
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreatePSP arms a PSP. Credentials are write-only and are checked with the
// provider before anything is stored. Retry with the same OperationID and
// params to read the first result.
func (c *Client) CreatePSP(ctx context.Context, params billing.CreatePSPParams, requestOptions ...RequestOption) (*billing.PSP, error) {
	var out billing.PSP
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/psps", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdatePSP changes a PSP's settings or rotates its credentials; new
// credentials are checked with the provider first. ExpectedRevision is the
// revision the caller read.
func (c *Client) UpdatePSP(ctx context.Context, id billing.PSPID, params billing.UpdatePSPParams, requestOptions ...RequestOption) (*billing.PSP, error) {
	path, err := pspPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.PSP
	if err := c.do(ctx, http.MethodPatch, path, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ArchivePSP archives a PSP: it takes no new work and keeps serving its
// existing subscriptions until they drain. No provider call is made.
func (c *Client) ArchivePSP(ctx context.Context, id billing.PSPID, params billing.ArchivePSPParams, requestOptions ...RequestOption) (*billing.PSP, error) {
	path, err := pspPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.PSP
	if err := c.do(ctx, http.MethodPost, path+"/archive", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// PreviewPSPRouting reports which PSP a checkout for a price would use, and
// why each other PSP was passed over. Nothing is created.
func (c *Client) PreviewPSPRouting(ctx context.Context, params billing.PreviewPSPRoutingParams, requestOptions ...RequestOption) (*billing.PSPRoutingPreview, error) {
	var out billing.PSPRoutingPreview
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/psps/routing-preview", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// RefreshPSPs pulls the merchant's PSPs now: provider-side renewals, declines,
// cancellations and vault changes, the pull that otherwise runs on a
// schedule.
func (c *Client) RefreshPSPs(ctx context.Context, requestOptions ...RequestOption) (*billing.PSPRefresh, error) {
	var out billing.PSPRefresh
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/psps/refresh", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRails lists the rails a merchant can arm a PSP on, with the credential
// and setting keys a PSP on each takes.
func (c *Client) ListRails(ctx context.Context, requestOptions ...RequestOption) (*billing.ListPage[billing.RailDefinition], error) {
	var out billing.ListPage[billing.RailDefinition]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/rails", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func pspPath(id billing.PSPID) (string, error) {
	if id.IsZero() {
		return "", invalidErr("PSP id is required")
	}
	return "/v1/merchant/psps/" + url.PathEscape(id.String()), nil
}
