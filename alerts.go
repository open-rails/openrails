package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/open-rails/openrails/billing"
)

// cursorQuery is a cursor-paged list's query string.
func cursorQuery(page billing.PageRequest) url.Values {
	q := url.Values{}
	if page.Limit > 0 {
		q.Set("limit", strconv.Itoa(page.Limit))
	}
	if page.Cursor != "" {
		q.Set("cursor", page.Cursor)
	}
	return q
}

// ListAlertWebhooks returns where the merchant's operational alerts are posted.
func (c *Client) ListAlertWebhooks(ctx context.Context, params billing.AlertWebhookListParams, options ...RequestOption) (*billing.ListPage[billing.AlertWebhook], error) {
	q := url.Values{}
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.AlertWebhook]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/admin/alert-webhooks", q), nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateAlertWebhook adds a destination for the merchant's operational alerts.
func (c *Client) CreateAlertWebhook(ctx context.Context, req billing.CreateAlertWebhookParams, options ...RequestOption) (*billing.AlertWebhook, error) {
	var out billing.AlertWebhook
	if err := c.do(ctx, http.MethodPost, "/v1/admin/alert-webhooks", req, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateAlertWebhook changes an alert webhook's URL, name, format or enabled
// state; omitted fields keep their values.
func (c *Client) UpdateAlertWebhook(ctx context.Context, id billing.AlertWebhookID, req billing.UpdateAlertWebhookParams, options ...RequestOption) (*billing.AlertWebhook, error) {
	if id.IsZero() {
		return nil, invalidErr("alert webhook id is required")
	}
	var out billing.AlertWebhook
	if err := c.do(ctx, http.MethodPatch, "/v1/admin/alert-webhooks/"+id.String(), req, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteAlertWebhook removes an alert webhook.
func (c *Client) DeleteAlertWebhook(ctx context.Context, id billing.AlertWebhookID, options ...RequestOption) error {
	if id.IsZero() {
		return invalidErr("alert webhook id is required")
	}
	return c.do(ctx, http.MethodDelete, "/v1/admin/alert-webhooks/"+id.String(), nil, nil, options...)
}
