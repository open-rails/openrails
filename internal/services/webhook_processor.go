package services

import (
	"context"
	"fmt"

	"github.com/doujins-org/doujins-billing/internal/db/models"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// WebhookProcessor orchestrates loading, dispatching, and updating webhook events.
// Processing is now synchronous-only - no retry mechanism.
type WebhookProcessor struct {
	Events     *WebhookEventService
	Dispatcher *WebhookDispatcher
}

// Process executes the webhook event identified by id.
// Returns error if processing fails - caller should log but may still return success
// to payment processor since they will retry on their end.
func (p *WebhookProcessor) Process(ctx context.Context, id uuid.UUID) error {
	if p == nil || p.Events == nil || p.Dispatcher == nil {
		return fmt.Errorf("webhook processor not initialised")
	}

	event, err := p.Events.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get webhook event: %w", err)
	}
	if event.Status == WebhookStatusProcessed {
		log.WithContext(ctx).WithFields(log.Fields{
			"event_id":  event.ID,
			"processor": event.Processor,
			"eventType": event.EventType,
		}).Info("webhook already processed; skipping")
		return nil
	}

	if err := p.Dispatcher.Process(ctx, event); err != nil {
		// Mark as failed for audit trail
		if markErr := p.Events.MarkFailed(ctx, id, err); markErr != nil {
			return fmt.Errorf("processing failed and could not mark failure: %w (original: %v)", markErr, err)
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"event_id":  event.ID,
			"processor": event.Processor,
			"eventType": event.EventType,
		}).WithError(err).Error("webhook processing failed")
		return err
	}

	if err := p.Events.MarkProcessed(ctx, id); err != nil {
		return fmt.Errorf("mark webhook success: %w", err)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"event_id":  event.ID,
		"processor": event.Processor,
		"eventType": event.EventType,
	}).Info("webhook processed")
	return nil
}

// ProcessDirect processes a webhook event directly without requiring it to be stored first.
// This is for simplified synchronous processing.
func (p *WebhookProcessor) ProcessDirect(ctx context.Context, event *models.WebhookEvent) error {
	if p == nil || p.Dispatcher == nil {
		return fmt.Errorf("webhook processor not initialised")
	}

	return p.Dispatcher.Process(ctx, event)
}
