package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// ListProposals lists the merchant's proposals: open pull.* findings unless
// Status is set, most severe first. Pull names the PSP import whose pull
// raised them.
func (c *Client) ListProposals(ctx context.Context, params billing.ProposalListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Finding], error) {
	q := cursorQuery(params.PageRequest)
	for key, value := range map[string]string{"kind": string(params.Kind), "source": string(params.Source), "status": string(params.Status)} {
		if value != "" {
			q.Set(key, value)
		}
	}
	if !params.PSPID.IsZero() {
		q.Set("psp_id", params.PSPID.String())
	}
	if !params.Pull.IsZero() {
		q.Set("pull_id", params.Pull.String())
	}
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.Finding]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/proposals?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ApproveProposals applies 1 to 100 proposals, each in its own transaction,
// under one approval run that `openrails undo-run` reverses. Only a person
// signed in, or the in-process host, approves. It needs WithIdempotencyKey:
// a retry with the key answers the first call's results.
func (c *Client) ApproveProposals(ctx context.Context, params billing.ApproveProposalsParams, requestOptions ...RequestOption) (*billing.ProposalResults, error) {
	key, err := requiredIdempotencyKey("", requestOptions)
	if err != nil {
		return nil, err
	}
	var out billing.ProposalResults
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/proposals/approve", params, &out, http.Header{"Idempotency-Key": {key}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DismissProposals closes 1 to 100 proposals without applying them; notes
// are required. Only a person signed in, or the in-process host, dismisses.
func (c *Client) DismissProposals(ctx context.Context, params billing.DismissProposalsParams, requestOptions ...RequestOption) (*billing.ProposalResults, error) {
	var out billing.ProposalResults
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/proposals/dismiss", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
