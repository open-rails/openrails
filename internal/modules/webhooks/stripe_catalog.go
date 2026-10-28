package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
)

// ObserveRequest names one provider catalog object an event announced.
type ObserveRequest struct {
	MerchantID uuid.UUID
	PSPID      uuid.UUID
	// Kind is product or price.
	Kind      string
	Ref       string
	EventType string
}

// ProviderObjectObserveEnqueuer schedules the coalesced read of one catalog
// object. Implemented by the River producer.
type ProviderObjectObserveEnqueuer interface {
	EnqueueProviderObjectObserve(ctx context.Context, req ObserveRequest) error
}

// markCatalogObjectDirty parses only the object's id and queues its read.
// Nothing is written to Stripe; an unavailable enqueuer is retryable.
func (s *StripeWebhookService) markCatalogObjectDirty(ctx context.Context, eventType, kind string, obj json.RawMessage) error {
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(obj, &data); err != nil {
		return fmt.Errorf("parse %s event: %w", kind, err)
	}
	ref := strings.TrimSpace(data.ID)
	if ref == "" {
		log.WithContext(ctx).WithField("event_type", eventType).Info("stripe catalog event without an object id; nothing to observe")
		return nil
	}
	if s.ObserveEnqueuer == nil {
		return fmt.Errorf("observe %s %s: enqueuer unavailable", kind, ref)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return err
	}
	return s.ObserveEnqueuer.EnqueueProviderObjectObserve(ctx, ObserveRequest{MerchantID: mid.UUID(), PSPID: pspID, Kind: kind, Ref: ref, EventType: eventType})
}
