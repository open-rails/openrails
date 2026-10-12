package openrails

import (
	"context"
	"net/http"
	"strconv"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/notificationevents"
)

// NotificationConsumer is an independent durable delivery subscription. Hosts
// persist or deliberately skip an event before acknowledging it. Concurrent
// readers may receive duplicates, so destination writes must be idempotent.
type NotificationConsumer struct {
	local  *notificationevents.Consumer
	client *Client
	name   string
}

// NotificationEvents binds this application's named merchant consumer. A stable
// application identity survives API key rotation; another application cannot
// acknowledge or retire it. Call Register once before polling Pending.
func (c *Client) NotificationEvents(name string) (*NotificationConsumer, error) {
	if !notificationevents.ValidName(name) {
		return nil, invalidErr("invalid notification consumer name")
	}
	return &NotificationConsumer{client: c, name: name}, nil
}

// NotificationEvents binds a trusted host's global consumer across merchants.
// This capability exists only in process, never on a remote endpoint.
func (e *Engine) NotificationEvents(name string) (*NotificationConsumer, error) {
	source, err := notificationevents.New(e.engine.App.Runtime.DB, nil, "host:global", name)
	if err != nil {
		return nil, err
	}
	return &NotificationConsumer{local: source, name: name}, nil
}
func (c *NotificationConsumer) path() string { return "/v1/app/notification-consumers/" + c.name }

// Register starts this consumer from the retained source facts, idempotently.
func (c *NotificationConsumer) Register(ctx context.Context) error {
	if c.local != nil {
		return c.local.Register(ctx)
	}
	return c.client.do(ctx, http.MethodPost, c.path(), nil, nil)
}

// Pending reads 1 to 100 pending events (zero means 100), with no watermark.
func (c *NotificationConsumer) Pending(ctx context.Context, limit int) ([]billing.NotificationEvent, error) {
	if c.local != nil {
		return c.local.Pending(ctx, limit)
	}
	var out billing.NotificationEvents
	err := c.client.do(ctx, http.MethodGet, c.path()+"/events?limit="+strconv.Itoa(limit), nil, &out)
	return out.Data, err
}

// Acknowledge records only this consumer's processing of the named facts.
func (c *NotificationConsumer) Acknowledge(ctx context.Context, refs []billing.NotificationEventRef) error {
	if c.local != nil {
		return c.local.Acknowledge(ctx, refs)
	}
	ids := make([]billing.NotificationID, 0, len(refs))
	for _, ref := range refs {
		if ref.MerchantID != c.client.MerchantID() {
			return invalidErr("notification event belongs to another merchant")
		}
		ids = append(ids, ref.ID)
	}
	if err := batchIDs("notification_ids", ids, 100); err != nil {
		return err
	}
	return c.client.do(ctx, http.MethodPost, c.path()+"/acknowledge", billing.AcknowledgeNotificationEventsParams{NotificationIDs: ids}, nil)
}

// Retire explicitly releases this consumer's retention claim.
func (c *NotificationConsumer) Retire(ctx context.Context) error {
	if c.local != nil {
		return c.local.Retire(ctx)
	}
	return c.client.do(ctx, http.MethodDelete, c.path(), nil, nil)
}
