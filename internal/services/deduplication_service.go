package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	log "github.com/sirupsen/logrus"
)

// DeduplicationService provides robust webhook deduplication using the unified IdempotencyService
type DeduplicationService struct {
	idem *IdempotencyService
	db   *db.DB
}

// NonRetryableWebhookError marks a processing failure as terminal.
// ProcessWebhook will mark idempotency as success and stop retries for this error.
type NonRetryableWebhookError struct {
	Err error
}

func (e *NonRetryableWebhookError) Error() string {
	if e == nil || e.Err == nil {
		return "non-retryable webhook error"
	}
	return e.Err.Error()
}

func (e *NonRetryableWebhookError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// MarkWebhookErrorNonRetryable wraps err so ProcessWebhook treats it as terminal.
func MarkWebhookErrorNonRetryable(err error) error {
	if err == nil {
		return nil
	}
	var existing *NonRetryableWebhookError
	if errors.As(err, &existing) {
		return err
	}
	return &NonRetryableWebhookError{Err: err}
}

func isWebhookErrorNonRetryable(err error) bool {
	var nonRetryable *NonRetryableWebhookError
	return errors.As(err, &nonRetryable)
}

// NewDeduplicationService creates a new webhook deduplication service
func NewDeduplicationService(idem *IdempotencyService, database *db.DB) *DeduplicationService {
	return &DeduplicationService{idem: idem, db: database}
}

// IsDuplicate checks if a webhook with this eventID has already been processed successfully.
// Returns true if the webhook should be skipped (already processed), false otherwise.
func (s *DeduplicationService) IsDuplicate(ctx context.Context, processor string, eventID string) (bool, error) {
	trimmedEventID := strings.TrimSpace(eventID)
	if trimmedEventID == "" {
		return false, nil // No event ID, can't deduplicate
	}

	op := fmt.Sprintf("webhook.%s.event", processor)

	if s.hasDurableStore() {
		status, exists, err := s.beginDurable(ctx, op, trimmedEventID)
		if err != nil {
			return false, fmt.Errorf("failed to check durable webhook idempotency: %w", err)
		}
		if exists && status == IdempotencyStatusSuccess {
			return true, nil
		}
		return false, nil
	}

	rec, alreadyExists, err := s.idem.Begin(ctx, op, trimmedEventID)
	if err != nil {
		return false, fmt.Errorf("failed to check idempotency: %w", err)
	}
	if alreadyExists && rec.Status == IdempotencyStatusSuccess {
		return true, nil // Already processed successfully
	}

	// If this call claimed the event ID, leave it pending.
	// Callers must transition pending -> success/failed after processing.

	return false, nil
}

// ProcessWebhook handles webhook deduplication and processing coordination.
func (s *DeduplicationService) ProcessWebhook(ctx context.Context, eventID, eventType string, processor models.Processor, payload interface{}, processingFunc func(ctx context.Context) error) error {
	var payloadBytes []byte
	if payload != nil {
		if data, err := json.Marshal(payload); err == nil {
			payloadBytes = data
		} else {
			log.WithContext(ctx).WithError(err).Warn("failed to marshal webhook payload for idempotency storage")
		}
	}

	trimmedEventID := strings.TrimSpace(eventID)
	op := fmt.Sprintf("webhook.%s.%s", processor, eventType)

	var shouldRecordOutcome bool
	if trimmedEventID != "" {
		if s.hasDurableStore() {
			status, alreadyExists, err := s.beginDurable(ctx, op, trimmedEventID)
			if err != nil {
				return fmt.Errorf("failed to begin durable webhook idempotency: %w", err)
			}
			if alreadyExists && status == IdempotencyStatusSuccess {
				log.WithContext(ctx).WithFields(log.Fields{
					"eventID":   trimmedEventID,
					"eventType": eventType,
					"processor": processor,
				}).Info("Webhook already processed successfully, skipping")
				return nil
			}
			shouldRecordOutcome = true
		} else {
			rec, alreadyExists, err := s.idem.Begin(ctx, op, trimmedEventID)
			if err != nil {
				return fmt.Errorf("failed to begin idempotency: %w", err)
			}
			if alreadyExists && rec.Status == IdempotencyStatusSuccess {
				log.WithContext(ctx).WithFields(log.Fields{
					"eventID":   trimmedEventID,
					"eventType": eventType,
					"processor": processor,
				}).Info("Webhook already processed successfully, skipping")
				return nil
			}
			shouldRecordOutcome = rec == nil || rec.Status != IdempotencyStatusSuccess
		}
	}

	processingErr := processingFunc(ctx)
	if processingErr != nil {
		nonRetryable := isWebhookErrorNonRetryable(processingErr)

		log.WithContext(ctx).WithFields(log.Fields{
			"eventID":   trimmedEventID,
			"eventType": eventType,
			"processor": processor,
			"error":     processingErr.Error(),
		}).Error("Webhook processing failed")

		if shouldRecordOutcome && trimmedEventID != "" {
			if nonRetryable {
				if s.hasDurableStore() {
					if err := s.completeDurable(ctx, op, trimmedEventID, payloadBytes); err != nil {
						log.WithContext(ctx).WithError(err).Warn("failed to mark non-retryable durable webhook idempotency as complete")
					}
				} else if err := s.idem.Complete(ctx, op, trimmedEventID, payloadBytes); err != nil {
					log.WithContext(ctx).WithError(err).Warn("failed to mark non-retryable webhook idempotency as complete")
				}
			} else {
				if s.hasDurableStore() {
					if err := s.failDurable(ctx, op, trimmedEventID, processingErr); err != nil {
						log.WithContext(ctx).WithError(err).Warn("failed to mark durable webhook idempotency as failed")
					}
				} else if err := s.idem.Fail(ctx, op, trimmedEventID, processingErr); err != nil {
					log.WithContext(ctx).WithError(err).Warn("failed to mark webhook idempotency as failed")
				}
			}
		}

		if nonRetryable {
			log.WithContext(ctx).WithFields(log.Fields{
				"eventID":   trimmedEventID,
				"eventType": eventType,
				"processor": processor,
			}).Warn("Webhook failed with non-retryable error; marked complete to avoid futile retries")
			return nil
		}

		return fmt.Errorf("webhook processing failed: %w", processingErr)
	}

	if shouldRecordOutcome && trimmedEventID != "" {
		if s.hasDurableStore() {
			if err := s.completeDurable(ctx, op, trimmedEventID, payloadBytes); err != nil {
				log.WithContext(ctx).WithError(err).Warn("failed to mark durable webhook idempotency as complete")
			}
		} else if err := s.idem.Complete(ctx, op, trimmedEventID, payloadBytes); err != nil {
			log.WithContext(ctx).WithError(err).Warn("failed to mark webhook idempotency as complete")
		}
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"eventID":   trimmedEventID,
		"eventType": eventType,
		"processor": processor,
	}).Info("Webhook processed successfully")

	return nil
}

// GetFailedWebhooks retrieves webhooks that failed and can be retried
// Not supported with TTL-based storage - failed webhooks expire automatically
func (s *DeduplicationService) GetFailedWebhooks(ctx context.Context, processor models.Processor, limit int) ([]any, error) {
	return []any{}, nil
}

func (s *DeduplicationService) CleanupOldWebhooks(ctx context.Context, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, fmt.Errorf("retentionDays must be > 0")
	}

	if !s.hasDurableStore() {
		log.WithContext(ctx).WithFields(log.Fields{
			"retentionDays": retentionDays,
		}).Debug("No durable dedupe store configured; skipping webhook cleanup")
		return 0, nil
	}

	query := `
DELETE FROM billing.webhook_idempotency_backup
WHERE last_seen_at < (NOW() - (? * INTERVAL '1 day'))
`
	res, err := s.db.GetDB().NewRaw(query, retentionDays).Exec(ctx)
	if err != nil {
		return 0, err
	}
	rows, _ := res.RowsAffected()

	log.WithContext(ctx).WithFields(log.Fields{
		"retentionDays": retentionDays,
		"deletedRows":   rows,
	}).Info("Cleaned up old durable webhook dedupe records")
	return rows, nil
}

func (s *DeduplicationService) hasDurableStore() bool {
	return s != nil && s.db != nil && s.db.GetDB() != nil
}

type durableIdempotencyRow struct {
	Status   string `bun:"status"`
	Inserted bool   `bun:"inserted"`
}

func (s *DeduplicationService) beginDurable(ctx context.Context, operation, key string) (IdempotencyStatus, bool, error) {
	var row durableIdempotencyRow
	query := `
WITH inserted AS (
	INSERT INTO billing.webhook_idempotency_backup (
		operation, idempotency_key, status, first_seen_at, last_seen_at, created_at, updated_at
	)
	VALUES (?, ?, ?, NOW(), NOW(), NOW(), NOW())
	ON CONFLICT (operation, idempotency_key) DO NOTHING
	RETURNING status, true AS inserted
)
SELECT status, inserted FROM inserted
UNION ALL
SELECT status, false AS inserted
FROM billing.webhook_idempotency_backup
WHERE operation = ? AND idempotency_key = ?
  AND NOT EXISTS (SELECT 1 FROM inserted)
LIMIT 1
`
	if err := s.db.GetDB().NewRaw(query, operation, key, string(IdempotencyStatusPending), operation, key).Scan(ctx, &row); err != nil {
		return "", false, err
	}
	return IdempotencyStatus(strings.TrimSpace(row.Status)), !row.Inserted, nil
}

func (s *DeduplicationService) completeDurable(ctx context.Context, operation, key string, result json.RawMessage) error {
	query := `
INSERT INTO billing.webhook_idempotency_backup (
	operation, idempotency_key, status, payload, error, first_seen_at, last_seen_at, created_at, updated_at
)
VALUES (?, ?, ?, ?::jsonb, NULL, NOW(), NOW(), NOW(), NOW())
ON CONFLICT (operation, idempotency_key) DO UPDATE
SET status = EXCLUDED.status,
	payload = EXCLUDED.payload,
	error = NULL,
	last_seen_at = NOW(),
	updated_at = NOW()
`
	payload := "null"
	if len(result) > 0 {
		payload = string(result)
	}
	_, err := s.db.GetDB().NewRaw(query, operation, key, string(IdempotencyStatusSuccess), payload).Exec(ctx)
	return err
}

func (s *DeduplicationService) failDurable(ctx context.Context, operation, key string, failure error) error {
	errMsg := ""
	if failure != nil {
		errMsg = failure.Error()
	}
	query := `
INSERT INTO billing.webhook_idempotency_backup (
	operation, idempotency_key, status, payload, error, first_seen_at, last_seen_at, created_at, updated_at
)
VALUES (?, ?, ?, NULL, ?, NOW(), NOW(), NOW(), NOW())
ON CONFLICT (operation, idempotency_key) DO UPDATE
SET status = EXCLUDED.status,
	error = EXCLUDED.error,
	last_seen_at = NOW(),
	updated_at = NOW()
`
	_, err := s.db.GetDB().NewRaw(query, operation, key, string(IdempotencyStatusFailed), errMsg).Exec(ctx)
	return err
}
