//go:build integration

package catalog

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/stretchr/testify/require"
)

type webhookQualificationWire func(*http.Request) (*http.Response, error)

func (f webhookQualificationWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failedWebhookPublication struct{ StripeWebhookPublisher }

func (p failedWebhookPublication) Publish(context.Context, string, string) error {
	return errors.New("synthetic failure before SQL publication")
}

func TestManagedWebhookPublishesExactReferencesAndRetiresOverlap(t *testing.T) {
	ctx := context.Background()
	d := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	store, err := merchants.NewDBSecretStore(d.DataPool())
	require.NoError(t, err)
	service, err := merchants.NewService(d.DataPool(), store, "test")
	require.NoError(t, err)
	account := "acct_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	service.StripeClients = stripeapi.NewFactory(webhookQualificationWire(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodGet, r.Method)
		body := `{"object":"balance","livemode":false}`
		switch r.URL.Path {
		case "/v1/account":
			body = `{"object":"account","id":"` + account + `"}`
		case "/v1/balance":
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	merchant, _, err := service.Provision(ctx, merchants.ProvisionRequest{Slug: "webhook-publish-" + uuid.NewString(), PermissionGroupID: "group-" + uuid.NewString()})
	require.NoError(t, err)
	initial, err := service.UpsertPaymentProviderConfig(ctx, merchant.ID, "stripe", merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: new(int64), AccountID: account, Credentials: map[string]string{"secret_key": "sk_test_publication", "webhook_signing_secret": "whsec_original"}})
	require.NoError(t, err)
	fake := newFakeStripeWebhooks()
	provider := newWebhookTestSvc(t, fake)
	now := time.Now().UTC()
	fake.endpoints["we_old"] = &StripeWebhookEndpoint{ID: "we_old", URL: "https://billing.example/old", Status: "enabled", APIVersion: "2020-01-01", Created: 1, EnabledEvents: []string{"invoice.paid"}, Metadata: map[string]string{StripeMetadataOpenRailsManaged: "true"}}
	params := ManagedStripeWebhookParams{Config: &config.Config{PublicBillingBaseURL: "https://billing.example", ProviderWriteMode: config.ProviderWriteModeFull}, MerchantID: merchant.ID, ProviderEnvironment: "test", PspID: account, EnabledEvents: []string{"invoice.paid"}, StripeBaseURL: provider.BaseURL, Now: now}
	params.Publication = failedWebhookPublication{service.StripeWebhookPublication(merchant.ID, account)}
	_, err = ReconcileManagedStripeWebhook(ctx, params)
	require.ErrorContains(t, err, "synthetic failure before SQL")
	old, ok, err := service.LoadStripeCredentialsForAccount(ctx, merchant.ID, account)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "whsec_original", old.WebhookSigningSecret)
	require.Empty(t, old.WebhookSigningPrevious)
	require.Empty(t, fake.endpoints["we_old"].Metadata[StripeMetadataSupersededAt])
	current, err := service.GetPaymentProviderConfig(ctx, merchant.ID, "stripe", "test")
	require.NoError(t, err)
	require.Equal(t, initial.Revision, current.Revision)
	// The failed create exists at the provider, but is never mistaken for
	// published custody merely because the old signing secret remains available.
	params.Publication = service.StripeWebhookPublication(merchant.ID, account)
	result, err := ReconcileManagedStripeWebhook(ctx, params)
	require.NoError(t, err)
	require.Equal(t, 2, fake.creates)
	require.Equal(t, "we_1", result.Result.EndpointID)
	loaded, _, err := service.LoadStripeCredentialsForAccount(ctx, merchant.ID, account)
	require.NoError(t, err)
	require.Equal(t, "whsec_fake_1", loaded.WebhookSigningSecret)
	require.Equal(t, "whsec_original", loaded.WebhookSigningPrevious)

	// Independent loader used by the webhook handler sees the atomic pair.
	other, err := merchants.NewService(d.DataPool(), store, "test")
	require.NoError(t, err)
	loaded, _, err = other.LoadStripeCredentialsForAccount(ctx, merchant.ID, account)
	require.NoError(t, err)
	require.Equal(t, "whsec_fake_1", loaded.WebhookSigningSecret)
	params.Now = now.Add(WebhookRolloverOverlap + time.Hour)
	params.AllowRetire = true
	params.Publication = service.StripeWebhookPublication(merchant.ID, account)
	result, err = ReconcileManagedStripeWebhook(ctx, params)
	require.NoError(t, err)
	require.Len(t, result.Retired, 2)
	loaded, _, err = other.LoadStripeCredentialsForAccount(ctx, merchant.ID, account)
	require.NoError(t, err)
	require.Empty(t, loaded.WebhookSigningPrevious)
	// Another version rollover is now allowed; retirement did not delete
	// historical candidates, and its reference removal cannot resurrect them.
	fake.endpoints["we_1"].APIVersion = "2020-01-01"
	params.Publication = service.StripeWebhookPublication(merchant.ID, account)
	_, err = ReconcileManagedStripeWebhook(ctx, params)
	require.NoError(t, err)
	loaded, _, err = other.LoadStripeCredentialsForAccount(ctx, merchant.ID, account)
	require.NoError(t, err)
	require.Equal(t, "whsec_fake_2", loaded.WebhookSigningSecret)
	require.Equal(t, "whsec_fake_1", loaded.WebhookSigningPrevious)
}
