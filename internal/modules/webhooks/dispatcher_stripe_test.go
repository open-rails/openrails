package webhooks

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/stretchr/testify/require"
)

func TestStripeWebhookHandlerBuildsPaymentStateReaderForRoutedAccount(t *testing.T) {
	t.Parallel()

	verified := true
	var selectedSecret string
	dispatcher := &WebhookDispatcher{
		RailConfigs: railresolve.FixedSet{
			"stripe_a": {
				Rail: models.RailStripe, AccountID: "acct_a",
				Stripe: &config.StripeRailConfig{SecretKey: "sk_test_a"},
			},
			"stripe_b": {
				Rail: models.RailStripe, AccountID: "acct_b",
				Stripe: &config.StripeRailConfig{SecretKey: "sk_test_b"},
			},
		},
		StripePaymentStateReaderFactory: func(secretKey string) payments.StripePaymentStateReader {
			selectedSecret = secretKey
			return stripePaymentStateStub{}
		},
	}
	event := &WebhookMessage{
		Rail:           string(models.RailStripe),
		EventType:      "customer.updated",
		PspID:          "acct_b",
		SignatureValid: &verified,
		Payload:        []byte(`{"id":"evt_1","type":"customer.updated","data":{"object":{"id":"cus_1"}}}`),
	}

	err := (StripeWebhookHandler{}).Apply(context.Background(), dispatcher, event)
	require.ErrorContains(t, err, "database is not configured")
	require.Equal(t, "sk_test_b", selectedSecret)
}

func TestStripeWebhookHandlerDoesNotResolveCredentialsForUnrelatedEvent(t *testing.T) {
	t.Parallel()

	verified := true
	dispatcher := &WebhookDispatcher{}
	event := &WebhookMessage{
		EventType:      "product.updated",
		SignatureValid: &verified,
		Payload:        []byte(`{"id":"evt_1","type":"product.updated","data":{"object":{"id":"prod_1"}}}`),
	}

	require.NoError(t, (StripeWebhookHandler{}).Apply(context.Background(), dispatcher, event))
}

type stripePaymentStateStub struct{}

func (stripePaymentStateStub) PaymentMethod(context.Context, string) (*payments.StripePaymentMethodState, error) {
	return nil, nil
}

func (stripePaymentStateStub) CustomerPaymentState(context.Context, string) (*payments.StripeCustomerPaymentState, error) {
	return nil, nil
}
