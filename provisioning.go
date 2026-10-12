package openrails

import (
	"context"
	"net/http"
	"net/url"

	"github.com/open-rails/openrails/billing"
)

// ListProvisioningTokens lists the merchant's provisioning tokens, oldest
// first; their secrets are never read back.
func (c *Client) ListProvisioningTokens(ctx context.Context, params billing.ProvisioningTokenListParams, options ...RequestOption) (*billing.ListPage[billing.ProvisioningToken], error) {
	q := url.Values{}
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.ProvisioningToken]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/merchant/provisioning-tokens", q), nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateProvisioningToken mints a token the merchant's directory presents at
// /scim/v2 as its bearer. The answer holds the token, which is never shown
// again.
func (c *Client) CreateProvisioningToken(ctx context.Context, req billing.CreateProvisioningTokenParams, options ...RequestOption) (*billing.CreatedProvisioningToken, error) {
	var out billing.CreatedProvisioningToken
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/provisioning-tokens", req, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteProvisioningToken revokes a provisioning token: the next request
// presenting it is refused.
func (c *Client) DeleteProvisioningToken(ctx context.Context, id billing.ProvisioningTokenID, options ...RequestOption) error {
	if id.IsZero() {
		return invalidErr("provisioning token id is required")
	}
	return c.do(ctx, http.MethodDelete, "/v1/merchant/provisioning-tokens/"+id.String(), nil, nil, options...)
}
