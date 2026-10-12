// Package notificationevents owns durable source delivery, not user inbox state.
package notificationevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// ValidName bounds a consumer's durable identity, independent of credentials.
func ValidName(name string) bool { return name != "." && name != ".." && namePattern.MatchString(name) }

// Consumer is a trusted binding. HTTP handlers derive owner from verified auth;
// only the embedding host can construct a nil-merchant global binding.
type Consumer struct {
	pool *db.Pool
	key  gen.GetNotificationConsumerParams
}

func New(database *db.DB, merchant *uuid.UUID, owner, name string) (*Consumer, error) {
	if !ValidName(name) || owner == "" || len(owner) > 512 {
		return nil, invalid("invalid notification consumer")
	}
	return &Consumer{pool: database.DataPool(), key: gen.GetNotificationConsumerParams{MerchantID: merchant, Owner: owner, Name: name}}, nil
}
func invalid(message string) error {
	return apperr.New(http.StatusBadRequest, billing.CodeInvalidParam, message)
}
func notRegistered(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.New(http.StatusNotFound, billing.CodeResourceNotFound, "notification consumer is not registered")
	}
	return err
}

// Register atomically joins the retained event set. Registration and GC share
// a fence; an event cannot disappear under a newly registered consumer.
func (c *Consumer) Register(ctx context.Context) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)
	if err = q.LockNotificationConsumers(ctx); err != nil {
		return err
	}
	if _, err = q.GetNotificationConsumer(ctx, c.key); err == nil {
		return tx.Commit(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	count, err := q.CountNotificationConsumers(ctx, gen.CountNotificationConsumersParams{MerchantID: c.key.MerchantID, Owner: c.key.Owner})
	if err != nil {
		return err
	}
	if count >= 32 {
		return invalid("at most 32 notification consumers per application and scope")
	}
	if err = q.RegisterNotificationConsumer(ctx, gen.RegisterNotificationConsumerParams(c.key)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Pending always scans unacknowledged committed facts. IDs and timestamps are
// ordering hints, never a high-water cursor that can skip a late commit.
func (c *Consumer) Pending(ctx context.Context, limit int) ([]billing.NotificationEvent, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 100 {
		return nil, invalid("limit must be between 1 and 100")
	}
	q := gen.New(c.pool)
	consumer, err := q.GetNotificationConsumer(ctx, c.key)
	if err != nil {
		return nil, notRegistered(err)
	}
	rows, err := q.PendingNotificationEvents(ctx, gen.PendingNotificationEventsParams{ConsumerID: consumer.ID, RowLimit: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]billing.NotificationEvent, 0, len(rows))
	for _, row := range rows {
		var policy struct {
			DowngradeApplied bool `json:"downgrade_applied"`
		}
		if err := json.Unmarshal(row.Data, &policy); err != nil {
			return nil, fmt.Errorf("decode notification event: %w", err)
		}
		out = append(out, billing.NotificationEvent{ID: billing.NotificationID(row.ID), MerchantID: billing.MerchantID(row.MerchantID), CustomerID: billing.CustomerID(row.CustomerID),
			EventType: row.EventType, Data: json.RawMessage(row.Data), CreatedAt: row.CreatedAt, ActionRequested: billing.CustomerNotificationActionRequested(row.EventType), Optional: billing.CustomerNotificationOptional(row.EventType, billing.NotificationData{DowngradeApplied: policy.DowngradeApplied})})
	}
	return out, nil
}

// Acknowledge records this consumer's durable processing, independently from
// every other consumer and any application's local read/dismiss state.
func (c *Consumer) Acknowledge(ctx context.Context, refs []billing.NotificationEventRef) error {
	if len(refs) == 0 || len(refs) > 100 {
		return invalid("acknowledge 1 to 100 notification events")
	}
	for _, ref := range refs {
		if ref.ID.IsZero() || ref.MerchantID.IsZero() || c.key.MerchantID != nil && ref.MerchantID.UUID() != *c.key.MerchantID {
			return invalid("invalid notification event reference")
		}
	}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)
	consumer, err := q.GetNotificationConsumer(ctx, c.key)
	if err != nil {
		return notRegistered(err)
	}
	for _, ref := range refs {
		if err = q.AcknowledgeNotificationEvent(ctx, gen.AcknowledgeNotificationEventParams{ConsumerID: consumer.ID, MerchantID: ref.MerchantID.UUID(), EventID: ref.ID.UUID()}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Retire releases this consumer's retention claim and acknowledgment history.
// Registering its name later starts again from whatever events remain retained.
func (c *Consumer) Retire(ctx context.Context) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)
	if err = q.LockNotificationConsumers(ctx); err != nil {
		return err
	}
	if err = q.RetireNotificationConsumer(ctx, gen.RetireNotificationConsumerParams(c.key)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Prune bounds one maintenance pass. Pending events never expire. Grace starts
// when all active merchant-scoped and global consumers have acknowledged.
func Prune(ctx context.Context, database *db.DB, now time.Time, limit int32) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, fmt.Errorf("notification prune limit must be 1 to 1000")
	}
	tx, err := database.DataPool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)
	if err = q.LockNotificationConsumers(ctx); err != nil {
		return 0, err
	}
	n, err := q.PruneNotificationEvents(ctx, gen.PruneNotificationEventsParams{Cutoff: now.Add(-30 * 24 * time.Hour), RowLimit: limit})
	if err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}
