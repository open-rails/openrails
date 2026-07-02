package repo

import (
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// Transitional shims (#688 phase 1): the gen<->models conversions moved to
// internal/db/models (models.*FromGen / models.ToJSONB / ...). These aliases
// keep the not-yet-relocated files in this package (subscription.go,
// notification_queue.go) untouched; they go away with those files in phase 2.

func fromJSONB[T any](b []byte, dst *T, col string) error { return models.FromJSONB(b, dst, col) }

func toJSONB[M ~map[string]V, V any](m M) ([]byte, error) { return models.ToJSONB(m) }

func subscriptionFromGen(s gen.OpenrailsSubscription) (*models.Subscription, error) {
	return models.SubscriptionFromGen(s)
}

func subscriptionsFromGen(rows []gen.OpenrailsSubscription) ([]*models.Subscription, error) {
	return models.SubscriptionsFromGen(rows)
}

func priceFromGen(p gen.OpenrailsPrice) (*models.Price, error) { return models.PriceFromGen(p) }

func productFromGen(p gen.OpenrailsProduct) (*models.Product, error) {
	return models.ProductFromGen(p)
}

func paymentMethodFromGen(p gen.OpenrailsPaymentMethod) (*models.PaymentMethod, error) {
	return models.PaymentMethodFromGen(p)
}
