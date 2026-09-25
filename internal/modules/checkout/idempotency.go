package checkout

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/modules/idempotency"
)

// idempotencyStore claims request keys durably (#1099).
type idempotencyStore interface {
	Begin(ctx context.Context, operation, key string) (*idempotency.Claim, *idempotency.Record, error)
}

type idempotencyCompleter interface {
	Complete(context.Context, json.RawMessage) error
}

func completeCheckoutIdempotency(ctx context.Context, claim idempotencyCompleter, operation, key string, result json.RawMessage) {
	if err := claim.Complete(ctx, result); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"operation":       operation,
			"idempotency_key": key,
		}).Error("checkout idempotency completion failed")
	}
}

func failCheckoutIdempotency(ctx context.Context, claim *idempotency.Claim, operation, key string, cause error) {
	if err := claim.Fail(ctx, cause); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"operation":       operation,
			"idempotency_key": key,
		}).Warn("checkout idempotency failure was not recorded")
	}
}

func GenerateKeyForSale(userID string, priceID uuid.UUID) string {
	return fmt.Sprintf("sale:%s:%s", userID, priceID)
}

func GenerateKeyForSubscription(userID string, priceID uuid.UUID) string {
	return fmt.Sprintf("subscription:%s:%s", userID, priceID)
}
