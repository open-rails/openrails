package checkout

import (
	"context"
	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNewSubscriptionCannotFallBackToProviderEnrollment(t *testing.T) {
	for _, cfg := range []*config.Config{nil, {}, {TestMode: config.CredentialPostureSandbox}} {
		service := &CheckoutService{Config: cfg}
		for _, rail := range []string{"stripe", "nmi", "ccbill", "solana"} {
			_, err := service.processSubscription(context.Background(), nil, nil, nil, nil, nil, rail)
			require.ErrorContains(t, err, "saved-method checkout session and explicit agreement confirmation")
		}
	}
}
