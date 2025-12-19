package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/doujins-org/doujins-billing/internal/db"
	"github.com/doujins-org/doujins-billing/internal/db/models"
	"github.com/google/uuid"
	"github.com/jackc/pgconn"
	"github.com/jonboulle/clockwork"
)

const (
	WebhookStatusPending   = "pending"
	WebhookStatusProcessed = "processed"
	WebhookStatusFailed    = "failed"
)

// WebhookEventService persists webhook events for audit logging.
// Webhook processing is now synchronous-only - no retry mechanism.
// Payment processors (CCBill, NMI) will retry failed webhooks from their end.
type WebhookEventService struct {
	db    *db.DB
	Clock clockwork.Clock
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *WebhookEventService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// NewWebhookEventService constructs a new persistence service.
func NewWebhookEventService(database *db.DB) *WebhookEventService {
	return &WebhookEventService{db: database}
}

// CreateWebhookEventParams holds the metadata for storing a webhook.
type CreateWebhookEventParams struct {
	Processor      string
	EventID        string
	EventType      string
	Payload        []byte
	Headers        map[string]string
	IPAddress      string
	Signature      string
	SignatureValid *bool
}

// Create stores the webhook event for audit logging and returns the persisted record.
func (s *WebhookEventService) Create(ctx context.Context, params CreateWebhookEventParams) (*models.WebhookEvent, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("webhook event service not initialised")
	}

	processor := strings.TrimSpace(strings.ToLower(params.Processor))
	if processor == "" {
		return nil, fmt.Errorf("processor is required")
	}
	if params.EventType == "" {
		return nil, fmt.Errorf("event type is required")
	}
	if params.IPAddress == "" {
		return nil, fmt.Errorf("ip address is required")
	}
	if params.EventID != "" {
		existing := new(models.WebhookEvent)
		err := s.db.GetDB().NewSelect().
			Model(existing).
			Where("processor = ? AND event_id = ?", processor, params.EventID).
			Limit(1).
			Scan(ctx)
		if err == nil {
			return existing, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("lookup webhook event: %w", err)
		}
	}

	event := &models.WebhookEvent{
		Processor:      processor,
		EventID:        nilIfEmpty(params.EventID),
		EventType:      params.EventType,
		Status:         WebhookStatusPending,
		RawPayload:     string(params.Payload),
		Headers:        params.Headers,
		IPAddress:      params.IPAddress,
		Signature:      nilIfEmpty(params.Signature),
		SignatureValid: params.SignatureValid,
	}

	if _, err := s.db.GetDB().NewInsert().Model(event).Returning("*").Exec(ctx); err != nil {
		if params.EventID != "" {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				existing := new(models.WebhookEvent)
				if selErr := s.db.GetDB().NewSelect().
					Model(existing).
					Where("processor = ? AND event_id = ?", processor, params.EventID).
					Limit(1).
					Scan(ctx); selErr == nil {
					return existing, nil
				}
			}
		}
		return nil, fmt.Errorf("insert webhook event: %w", err)
	}
	return event, nil
}

// Get fetches a webhook event by ID.
func (s *WebhookEventService) Get(ctx context.Context, id uuid.UUID) (*models.WebhookEvent, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("webhook event service not initialised")
	}
	event := new(models.WebhookEvent)
	if err := s.db.GetDB().NewSelect().Model(event).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return event, nil
}

// MarkProcessed marks the webhook as processed successfully.
func (s *WebhookEventService) MarkProcessed(ctx context.Context, id uuid.UUID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("webhook event service not initialised")
	}
	now := s.now()
	_, err := s.db.GetDB().NewUpdate().Model((*models.WebhookEvent)(nil)).
		Set("status = ?", WebhookStatusProcessed).
		Set("processed_at = ?", now).
		Set("updated_at = ?", now).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("mark webhook processed: %w", err)
	}
	return nil
}

// MarkFailed marks the webhook as failed with error message.
func (s *WebhookEventService) MarkFailed(ctx context.Context, id uuid.UUID, failure error) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("webhook event service not initialised")
	}
	now := s.now()
	_, err := s.db.GetDB().NewUpdate().Model((*models.WebhookEvent)(nil)).
		Set("status = ?", WebhookStatusFailed).
		Set("error_message = ?", truncateError(failure)).
		Set("processing_result = ?", map[string]any{"error": truncateError(failure)}).
		Set("updated_at = ?", now).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("mark webhook failed: %w", err)
	}
	return nil
}

func nilIfEmpty(val string) *string {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func truncateError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 2048 {
		return msg[:2048]
	}
	return msg
}
