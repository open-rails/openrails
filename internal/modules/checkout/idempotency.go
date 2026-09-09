package checkout

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

type idempotencyCompleter interface {
	Complete(context.Context, string, string, json.RawMessage) error
}

func completeCheckoutIdempotency(ctx context.Context, store idempotencyCompleter, operation, key string, result json.RawMessage) {
	if err := store.Complete(ctx, operation, key, result); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"operation":       operation,
			"idempotency_key": key,
		}).Error("checkout idempotency completion failed")
	}
}

func GenerateKeyForSale(userID string, priceID uuid.UUID) string {
	return fmt.Sprintf("sale:%s:%s", userID, priceID)
}

func GenerateKeyForSubscription(userID string, priceID uuid.UUID) string {
	return fmt.Sprintf("subscription:%s:%s", userID, priceID)
}

func GenerateKeyForUpgrade(userID string, oldSubscriptionID, newPriceID uuid.UUID) string {
	return fmt.Sprintf("upgrade:%s:%s:%s", userID, oldSubscriptionID, newPriceID)
}
