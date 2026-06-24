package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	log "github.com/sirupsen/logrus"
)

const (
	webhookPendingLease   = 2 * time.Minute
	WebhookIdempotencyTTL = 90 * 24 * time.Hour
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

func IsWebhookErrorNonRetryable(err error) bool {
	var nonRetryable *NonRetryableWebhookError
	return errors.As(err, &nonRetryable)
}

// NewDeduplicationService creates a new webhook deduplication service
func NewDeduplicationService(idem *idempotency.IdempotencyService) *DeduplicationService {
	return &DeduplicationService{idem: idem}
}

// IsDuplicate checks if a webhook with this eventID has already been processed successfully.
// Returns true if the webhook should be skipped (already processed), false otherwise.
func (s *DeduplicationService) IsDuplicate(ctx context.Context, rail string, eventID string) (bool, error) {
	trimmedEventID := strings.TrimSpace(eventID)
	if trimmedEventID == "" {
		return false, nil // No event ID, can't deduplicate
	}

	op := fmt.Sprintf("webhook.%s.event", rail)

	if s == nil || s.idem == nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"eventID": trimmedEventID,
			"rail":    rail,
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
func (s *DeduplicationService) ProcessWebhook(ctx context.Context, eventID, eventType string, rail models.Rail, payload interface{}, processingFunc func(ctx context.Context) error) error {
	var payloadBytes []byte
	if payload != nil {
		if data, err := json.Marshal(payload); err == nil {
			payloadBytes = data
		} else {
			log.WithContext(ctx).WithError(err).Warn("failed to marshal webhook payload for idempotency storage")
		}
	}

	trimmedEventID := strings.TrimSpace(eventID)
	op := fmt.Sprintf("webhook.%s.%s", rail, eventType)

	var shouldRecordOutcome bool
	if trimmedEventID != "" {
		if s == nil || s.idem == nil {
			log.WithContext(ctx).WithFields(log.Fields{
				"eventID":   trimmedEventID,
				"eventType": eventType,
				"rail":      rail,
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
					"rail":      rail,
				}).Info("Webhook already processed successfully, skipping")
				return nil
			}
			if alreadyExists && rec.Status == idempotency.IdempotencyStatusPending {
				if time.Since(rec.CreatedAt) > webhookPendingLease {
					taken, err := s.idem.TryTakeoverPending(ctx, op, trimmedEventID, webhookPendingLease)
					if err != nil {
						return fmt.Errorf("failed to take over stale webhook idempotency: %w", err)
					}
					if !taken {
						log.WithContext(ctx).WithFields(log.Fields{
							"eventID":   trimmedEventID,
							"eventType": eventType,
							"rail":      rail,
						}).Info("Stale pending webhook was claimed by another worker")
						return fmt.Errorf("webhook already in progress")
					}
					log.WithContext(ctx).WithFields(log.Fields{
						"eventID":   trimmedEventID,
						"eventType": eventType,
						"rail":      rail,
					}).Warn("Taking over stale pending webhook idempotency record")
					shouldRecordOutcome = true
				} else {
					log.WithContext(ctx).WithFields(log.Fields{
						"eventID":   trimmedEventID,
						"eventType": eventType,
						"rail":      rail,
					}).Info("Webhook already in progress, skipping concurrent duplicate")
					return fmt.Errorf("webhook already in progress")
				}
			} else {
				shouldRecordOutcome = rec == nil || rec.Status != idempotency.IdempotencyStatusSuccess
			}
		}
	}

	processingErr := processingFunc(ctx)
	if processingErr != nil {
		nonRetryable := IsWebhookErrorNonRetryable(processingErr)

		log.WithContext(ctx).WithFields(log.Fields{
			"eventID":   trimmedEventID,
			"eventType": eventType,
			"rail":      rail,
			"error":     processingErr.Error(),
		}).Error("Webhook processing failed")

		if shouldRecordOutcome && trimmedEventID != "" {
			if nonRetryable {
				if err := s.idem.Complete(ctx, op, trimmedEventID, payloadBytes); err != nil {
					return fmt.Errorf("failed to mark non-retryable webhook idempotency as complete: %w", err)
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
				"rail":      rail,
			}).Warn("Webhook failed with non-retryable error; marked complete to avoid futile retries")
			return nil
		}

		return fmt.Errorf("webhook processing failed: %w", processingErr)
	}

	if shouldRecordOutcome && trimmedEventID != "" {
		if err := s.idem.Complete(ctx, op, trimmedEventID, payloadBytes); err != nil {
			return fmt.Errorf("failed to mark webhook idempotency as complete: %w", err)
		}
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"eventID":   trimmedEventID,
		"eventType": eventType,
		"rail":      rail,
	}).Info("Webhook processed successfully")

	return nil
}
