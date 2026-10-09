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
func (c *Client) ListAlertWebhooks(ctx context.Context, options ...RequestOption) (*billing.ListPage[billing.AlertWebhook], error) {
	var out billing.ListPage[billing.AlertWebhook]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/alert-webhooks", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateAlertWebhook adds a destination for the merchant's operational alerts.
func (c *Client) CreateAlertWebhook(ctx context.Context, req billing.CreateAlertWebhookParams, options ...RequestOption) (*billing.AlertWebhook, error) {
	var out billing.AlertWebhook
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/alert-webhooks", req, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetAlertWebhookURL replaces an alert webhook's URL, keeping the webhook.
func (c *Client) SetAlertWebhookURL(ctx context.Context, id billing.AlertWebhookID, req billing.SetAlertWebhookURLParams, options ...RequestOption) (*billing.AlertWebhook, error) {
	if id.IsZero() {
		return nil, invalidErr("alert webhook id is required")
	}
	var out billing.AlertWebhook
	if err := c.do(ctx, http.MethodPut, "/v1/merchant/alert-webhooks/"+id.String()+"/url", req, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteAlertWebhook removes an alert webhook.
func (c *Client) DeleteAlertWebhook(ctx context.Context, id billing.AlertWebhookID, options ...RequestOption) error {
	if id.IsZero() {
		return invalidErr("alert webhook id is required")
	}
	return c.do(ctx, http.MethodDelete, "/v1/merchant/alert-webhooks/"+id.String(), nil, nil, options...)
}

// ListMerchantNotifications pages the merchant's inbox, newest first.
func (c *Client) ListMerchantNotifications(ctx context.Context, req billing.MerchantNotificationListParams, options ...RequestOption) (*billing.ListPage[billing.MerchantNotification], error) {
	q := cursorQuery(req.PageRequest)
	if req.UnreadOnly {
		q.Set("unread", "true")
	}
	var out billing.ListPage[billing.MerchantNotification]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/notifications?"+q.Encode(), nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetUnreadNotificationCount counts the merchant's unread notifications.
func (c *Client) GetUnreadNotificationCount(ctx context.Context, options ...RequestOption) (*billing.UnreadCount, error) {
	var out billing.UnreadCount
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/notifications/unread-count", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// MarkNotificationsRead marks 1 to billing.MaxBatchItems of the merchant's
// notifications read. Every requested notification is in the answer; one that
// does not exist is nil.
func (c *Client) MarkNotificationsRead(ctx context.Context, ids []billing.NotificationID, options ...RequestOption) (map[billing.NotificationID]*billing.MerchantNotification, error) {
	if err := batchIDs("notification_ids", ids, billing.MaxBatchItems); err != nil {
		return nil, err
	}
	var out billing.NotificationLookup
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/notifications/read", billing.MarkNotificationsReadParams{NotificationIDs: ids}, &out, options...); err != nil {
		return nil, err
	}
	return out.Notifications, nil
}
