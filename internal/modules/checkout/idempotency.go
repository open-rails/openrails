package checkout

import (
	"fmt"

	"github.com/google/uuid"
)

func GenerateKeyForSale(userID string, priceID uuid.UUID) string {
	return fmt.Sprintf("sale:%s:%s", userID, priceID)
}

func GenerateKeyForSubscription(userID string, priceID uuid.UUID) string {
	return fmt.Sprintf("subscription:%s:%s", userID, priceID)
}

func GenerateKeyForUpgrade(userID string, oldSubscriptionID, newPriceID uuid.UUID) string {
	return fmt.Sprintf("upgrade:%s:%s:%s", userID, oldSubscriptionID, newPriceID)
}
