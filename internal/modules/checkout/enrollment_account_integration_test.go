//go:build integration

package checkout

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/stretchr/testify/require"
)

type enrollmentReviewResolver struct{ client *nmi.NMIClient }

func (r enrollmentReviewResolver) ResolveNMIClient(context.Context, uuid.UUID, *uuid.UUID) (*nmi.NMIClient, bool, error) {
	return r.client, true, nil // Deliberately does not validate resolver provenance.
}

func TestEnrollmentReceiptRequiresActualReaderAccount(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	_, err := fx.upgrade(t)
	require.NoError(t, err)
	accepted := fx.operation(t)
	original, err := fx.svc.ResolveNMIClientOverride(fx.ctx, "nmi")
	require.NoError(t, err)
	for _, kind := range []string{"same account", "wrong PSP", "wrong merchant", "unscoped", "nil"} {
		t.Run(kind, func(t *testing.T) {
			var client *nmi.NMIClient
			var err error
			merchantID, pspID := accepted.MerchantID, *accepted.PspID
			cfg := &config.NMIProviderSettings{SecurityKey: "read-key-" + kind, WebhookSecret: "local-webhook-key"}
			switch kind {
			case "nil":
			case "unscoped":
				client, err = nmi.NewClient("nmi", cfg, true)
			default:
				if kind == "wrong PSP" {
					pspID = uuid.New()
				}
				if kind == "wrong merchant" {
					merchantID = uuid.New()
				}
				client, err = nmi.NewAccountClient(merchantID, pspID, "nmi", cfg, true)
			}
			require.NoError(t, err)
			if client != nil {
				// Every armed reader sees the exact same operation-correlated
				// schedule. Expected account labels cannot substitute for the
				// immutable account that actually supplied those provider facts.
				client.V5BaseURL, client.QueryURL = original.V5BaseURL, original.QueryURL
			}
			var receipt intents.NMIEnrollmentReceipt
			var found bool
			require.NotPanics(t, func() {
				receipt, found, err = intents.ReadNMIEnrollmentReceipt(fx.ctx, accepted, enrollmentReviewResolver{client}, fx.gateway.subID)
			})
			if kind == "same account" {
				require.NoError(t, err)
				require.True(t, found)
				require.NoError(t, receipt.Validate(accepted))
			} else {
				require.Error(t, err)
				require.False(t, found)
			}
		})
	}
}
