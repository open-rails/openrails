package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	log "github.com/sirupsen/logrus"
)

// DeduplicationService provides robust webhook deduplication using the unified IdempotencyService
type DeduplicationService struct {
	idem *idempotency.IdempotencyService
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
func NewDeduplicationService(idem *idempotency.IdempotencyService) *DeduplicationService {
	return &DeduplicationService{idem: idem}
}

// IsDuplicate checks if a webhook with this eventID has already been processed successfully.
// Returns true if the webhook should be skipped (already processed), false otherwise.
func (s *DeduplicationService) IsDuplicate(ctx context.Context, processor string, eventID string) (bool, error) {
	trimmedEventID := strings.TrimSpace(eventID)
	if trimmedEventID == "" {
		return false, nil // No event ID, can't deduplicate
	}

	op := fmt.Sprintf("webhook.%s.event", processor)

	if s == nil || s.idem == nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"eventID":   trimmedEventID,
			"processor": processor,
		}).Warn("IdempotencyService is not configured; dedupe check skipped")
		return false, nil
	}

	rec, alreadyExists, err := s.idem.Begin(ctx, op, trimmedEventID)
	if err != nil {
		return false, fmt.Errorf("failed to check idempotency: %w", err)
	}
	if alreadyExists && rec.Status == idempotency.IdempotencyStatusSuccess {
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
		if s == nil || s.idem == nil {
			log.WithContext(ctx).WithFields(log.Fields{
				"eventID":   trimmedEventID,
				"eventType": eventType,
				"processor": processor,
			}).Warn("IdempotencyService is not configured; processing webhook without dedupe protection")
		} else {
			rec, alreadyExists, err := s.idem.Begin(ctx, op, trimmedEventID)
			if err != nil {
				return fmt.Errorf("failed to begin idempotency: %w", err)
			}
			if alreadyExists && rec.Status == idempotency.IdempotencyStatusSuccess {
				log.WithContext(ctx).WithFields(log.Fields{
					"eventID":   trimmedEventID,
					"eventType": eventType,
					"processor": processor,
				}).Info("Webhook already processed successfully, skipping")
				return nil
			}
			shouldRecordOutcome = rec == nil || rec.Status != idempotency.IdempotencyStatusSuccess
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
				if err := s.idem.Complete(ctx, op, trimmedEventID, payloadBytes); err != nil {
					log.WithContext(ctx).WithError(err).Warn("failed to mark non-retryable webhook idempotency as complete")
				}
			} else {
				if err := s.idem.Fail(ctx, op, trimmedEventID, processingErr); err != nil {
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
		if err := s.idem.Complete(ctx, op, trimmedEventID, payloadBytes); err != nil {
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

	log.WithContext(ctx).WithFields(log.Fields{
		"retentionDays": retentionDays,
		"deletedRows":   0,
	}).Info("Durable webhook dedupe cleanup skipped (Postgres backup removed)")
	return 0, nil
}
