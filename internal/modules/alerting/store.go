package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
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
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return Webhook{}, queryScopeErr
	}

	row, err := s.db.Gen(ctx).RotateMerchantWebhookURL(ctx, gen.RotateMerchantWebhookURLParams{MerchantID: queryMerchant.UUID(), ID: id, DestinationHost: host, SecretVersion: version})
	if err != nil {
		return Webhook{}, err
	}
	return webhookFromRow(row), nil
}

func (s *store) getWebhook(ctx context.Context, id uuid.UUID) (Webhook, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return Webhook{}, queryScopeErr
	}

	row, err := s.db.Gen(ctx).GetMerchantWebhook(ctx, gen.GetMerchantWebhookParams{MerchantID: queryMerchant.UUID(), ID: id})
	if err != nil {
		return Webhook{}, err
	}
	return webhookFromRow(row), nil
}

func (s *store) listWebhooks(ctx context.Context, ids []uuid.UUID) ([]Webhook, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}
	var rows []gen.BillingMerchantWebhook
	var err error
	if ids != nil {
		rows, err = s.db.Gen(ctx).ListMerchantWebhooksByIDs(ctx, gen.ListMerchantWebhooksByIDsParams{MerchantID: queryMerchant.UUID(), Ids: ids})
	} else {
		rows, err = s.db.Gen(ctx).ListMerchantWebhooks(ctx, queryMerchant.UUID())
	}
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
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return 0, queryScopeErr
	}

	return s.db.Gen(ctx).DeleteMerchantWebhook(ctx, gen.DeleteMerchantWebhookParams{MerchantID: queryMerchant.UUID(), ID: id})
}

// --- notifications -----------------------------------------------------------

func (s *store) createNotification(ctx context.Context, n Notification) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	data, err := marshalJSON(n.Data, "")
	if err != nil {
		return err
	}
	if len(data) == 0 {
		data = nil
	}
	var id *uuid.UUID
	if n.ID != uuid.Nil {
		id = &n.ID
	}
	_, err = s.db.Gen(ctx).CreateMerchantNotification(ctx, gen.CreateMerchantNotificationParams{
		MerchantID: mid.UUID(),
		ID:         id,
		Severity:   string(n.Severity),
		Title:      n.Title,
		Body:       n.Body,
		Link:       n.Link,
		Data:       data,
	})
	return err
}

func (s *store) listNotifications(ctx context.Context, unreadOnly bool, afterAt *time.Time, afterID *uuid.UUID, limit int32) ([]gen.BillingNotification, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.db.Gen(ctx).ListMerchantNotifications(ctx, gen.ListMerchantNotificationsParams{
		MerchantID: mid.UUID(), UnreadOnly: unreadOnly, AfterAt: afterAt, AfterID: afterID, RowLimit: limit,
	})
}

func (s *store) notificationsByIDs(ctx context.Context, ids []uuid.UUID) ([]gen.BillingNotification, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.db.Gen(ctx).ListMerchantNotificationsByIDs(ctx, gen.ListMerchantNotificationsByIDsParams{MerchantID: mid.UUID(), Ids: ids})
}

func (s *store) markNotificationsRead(ctx context.Context, ids []uuid.UUID) ([]gen.BillingNotification, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.db.Gen(ctx).MarkMerchantNotificationsRead(ctx, gen.MarkMerchantNotificationsReadParams{MerchantID: mid.UUID(), Ids: ids})
}

func (s *store) unreadCount(ctx context.Context) (int64, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	return s.db.Gen(ctx).CountUnreadMerchantNotifications(ctx, mid.UUID())
}

// --- row mapping -------------------------------------------------------------

func webhookFromRow(row gen.BillingMerchantWebhook) Webhook {
	return Webhook{
		ID: row.ID, MerchantID: row.MerchantID, Name: row.Name, DestinationHost: row.DestinationHost, secretVersion: int(row.SecretVersion),
		Format: WebhookFormat(row.Format), Enabled: row.Enabled,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func notificationFromRow(row gen.BillingNotification) billing.MerchantNotification {
	n := billing.MerchantNotification{
		ID: billing.NotificationID(row.ID), Severity: billing.AlertSeverity(models.DerefStr(row.Severity)), Title: models.DerefStr(row.Title), Body: models.DerefStr(row.Body),
		Link: row.Link, CreatedAt: row.CreatedAt, ReadAt: row.ReadAt,
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
