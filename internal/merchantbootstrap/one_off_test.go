package merchantbootstrap

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controls"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/stripemock"
)

// A one-off tool's load of a merchant registers its Stripe PSPs' posture as
// a server's does, so the tool's write is sent on its PSP's check rather than
// refused for want of one.
func TestAOneOffToolsStripeWriteIsSentOnItsPSPsCheck(t *testing.T) {
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	svc := &merchants.Service{StripeClients: stripeapi.NewWiredFactory(stripe, controls.Fixed(controls.Full))}
	id := billing.MerchantID(uuid.New())
	name := strings.ReplaceAll(uuid.NewString(), "-", "")
	key := "sk_test_acct_" + name
	loaded := merchantdocs.Set{PSPs: map[string]merchantdocs.Doc[merchantdocs.PSP]{"stripe": {Value: merchantdocs.PSP{
		Rail: "stripe", Environment: "test", AccountID: "acct_" + name, Secrets: map[string]string{"secret_key": key}}}}}
	_, err := oneOffSync(svc)(context.Background(), id, merchantdocs.Set{}, loaded)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(merchant.WithID(context.Background(), id), http.MethodPost, stripeapi.APIBase+"/v1/refunds", strings.NewReader("payment_intent=pi_tool&amount=100"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	response, err := svc.StripeClients.Client(0).Do(req)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Len(t, stripe.Submitted("/v1/refunds"), 1)
}

func TestPSPDeclarationPreviousWebhookSecretWithoutOverlapExpiryIsIgnored(t *testing.T) {
	const previous = "previous-webhook-secret-at-least-32-characters"
	for _, tc := range []struct {
		name             string
		rail             billing.Rail
		previous, expiry string
		logged           bool
	}{
		{"missing expiry", billing.RailNMI, previous, "", true},
		{"invalid expiry", billing.RailCCBill, previous, "invalid", true},
		{"valid expiry", billing.RailNMI, previous, "2026-12-01T00:00:00Z", false},
		{"empty previous secret", billing.RailNMI, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })
			accountID, previousKey := "overlap", "webhook_signing_secret_previous"
			secrets := map[string]string{}
			if tc.rail == billing.RailCCBill {
				accountID, previousKey = "999999-0000", "webhook_token_previous"
				secrets["salt"] = "test-salt"
			}
			secrets[previousKey] = tc.previous
			settings := map[string]any{}
			if tc.expiry != "" {
				settings[merchants.WebhookOverlapExpiresKey] = tc.expiry
			}
			doc := billing.MerchantDocument{DisplayName: "Overlap", PSPs: billing.PSPs{"overlap": {
				Rail: tc.rail, AccountID: accountID, Secrets: secrets, Settings: settings,
			}}}
			served, refused := ScopeDeclaration(nil, doc)
			require.Empty(t, refused)
			require.Contains(t, served.PSPs, "overlap")
			if tc.logged {
				require.Contains(t, logs.String(), `level=ERROR msg="psps.overlap.webhook_overlap_expires_at (RFC 3339) is required with secrets.`+previousKey+`; the previous secret is ignored"`)
			}
		})
	}
}
