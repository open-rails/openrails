package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// store uses the merchant-pinned connection for notification and webhook data.
type store struct {
	db      *db.DB
	secrets merchants.MerchantSecretStore
}

func newStore(database *db.DB) *store { return &store{db: database} }

// --- webhooks ----------------------------------------------------------------

func (s *store) createWebhook(ctx context.Context, id uuid.UUID, name, host string, version int32, format WebhookFormat, enabled bool) (Webhook, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return Webhook{}, err
	}
	row, err := s.db.Gen(ctx).CreateMerchantWebhook(ctx, gen.CreateMerchantWebhookParams{
		ID: id, MerchantID: mid.UUID(), Name: name, DestinationHost: host, SecretVersion: version, Format: string(format), Enabled: enabled,
	})
	if err != nil {
		return Webhook{}, err
	}
	return webhookFromRow(row), nil
}

func (s *store) rotateWebhookURL(ctx context.Context, id uuid.UUID, host string, version int32) (Webhook, error) {
	row, err := s.db.Gen(ctx).RotateMerchantWebhookURL(ctx, gen.RotateMerchantWebhookURLParams{ID: id, DestinationHost: host, SecretVersion: version})
	if err != nil {
		return Webhook{}, err
	}
	return webhookFromRow(row), nil
}

func (s *store) getWebhook(ctx context.Context, id uuid.UUID) (Webhook, error) {
	row, err := s.db.Gen(ctx).GetMerchantWebhook(ctx, id)
	if err != nil {
		return Webhook{}, err
	}
	return webhookFromRow(row), nil
}

func (s *store) listWebhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := s.db.Gen(ctx).ListMerchantWebhooks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Webhook, 0, len(rows))
	for _, row := range rows {
		out = append(out, webhookFromRow(row))
	}
	return out, nil
}

func (s *store) deleteWebhook(ctx context.Context, id uuid.UUID) (int64, error) {
	return s.db.Gen(ctx).DeleteMerchantWebhook(ctx, id)
}

// --- notifications -----------------------------------------------------------

func (s *store) createNotification(ctx context.Context, n Notification) (Notification, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return Notification{}, err
	}
	data, err := marshalJSON(n.Data, "")
	if err != nil {
		return Notification{}, err
	}
	if len(data) == 0 {
		data = nil
	}
	row, err := s.db.Gen(ctx).CreateMerchantNotification(ctx, gen.CreateMerchantNotificationParams{
		MerchantID: mid.UUID(),
		Severity:   string(n.Severity),
		Title:      n.Title,
		Body:       n.Body,
		Link:       n.Link,
		Data:       data,
	})
	if err != nil {
		return Notification{}, err
	}
	return notificationFromRow(row), nil
}

func (s *store) listNotifications(ctx context.Context, unreadOnly bool, limit int) ([]Notification, error) {
	rows, err := s.db.Gen(ctx).ListMerchantNotifications(ctx, gen.ListMerchantNotificationsParams{
		UnreadOnly: unreadOnly, RowLimit: int32(limit), // #nosec G115 -- only caller passes the hardcoded notificationListLimit const (100); no HTTP param feeds limit
	})
	if err != nil {
		return nil, err
	}
	out := make([]Notification, 0, len(rows))
	for _, row := range rows {
		out = append(out, notificationFromRow(row))
	}
	return out, nil
}

func (s *store) markNotificationRead(ctx context.Context, id uuid.UUID) (int64, error) {
	return s.db.Gen(ctx).MarkMerchantNotificationRead(ctx, id)
}

func (s *store) unreadCount(ctx context.Context) (int64, error) {
	return s.db.Gen(ctx).CountUnreadMerchantNotifications(ctx)
}

// --- #787 reconciliation-findings notifications -----------------------------

// markFindingNotified stamps the finding's dedupe linkage after a successful
// (or attempted, for best-effort notification delivery)
// notification push.
func (s *store) markFindingNotified(ctx context.Context, id uuid.UUID, at time.Time, severity string) error {
	_, err := s.db.Gen(ctx).MarkReconciliationFindingNotified(ctx, gen.MarkReconciliationFindingNotifiedParams{
		ID: id, NotifiedAt: at, Severity: severity,
	})
	return err
}

// --- row mapping -------------------------------------------------------------

func webhookFromRow(row gen.OpenrailsMerchantWebhook) Webhook {
	return Webhook{
		ID: row.ID, MerchantID: row.MerchantID, Name: row.Name, DestinationHost: row.DestinationHost, secretVersion: int(row.SecretVersion),
		Format: WebhookFormat(row.Format), Enabled: row.Enabled,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func notificationFromRow(row gen.OpenrailsMerchantNotification) Notification {
	n := Notification{
		ID: row.ID, Severity: Severity(row.Severity), Title: row.Title, Body: row.Body,
		Link: row.Link, CreatedAt: row.CreatedAt, ReadAt: row.ReadAt,
	}
	if len(row.Data) > 0 {
		var d any
		if err := json.Unmarshal(row.Data, &d); err == nil {
			n.Data = d
		}
	}
	return n
}

// marshalJSON encodes v, substituting empty for a nil/zero value.
func marshalJSON(v any, empty string) ([]byte, error) {
	if v == nil {
		if empty == "" {
			return nil, nil
		}
		return []byte(empty), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("alerting: encode jsonb: %w", err)
	}
	if string(b) == "null" {
		if empty == "" {
			return nil, nil
		}
		return []byte(empty), nil
	}
	return b, nil
}
