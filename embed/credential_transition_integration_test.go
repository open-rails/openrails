//go:build integration

package embed_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/operator"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/stretchr/testify/require"
)

type custodyStripeWire func(*http.Request) (*http.Response, error)

func (f custodyStripeWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRuntimeCredentialCustodyTransitionAndSnapshotRestart(t *testing.T) {
	ctx := context.Background()
	_, dsn := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	account := "acct_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	wire := custodyStripeWire(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "api.stripe.com", r.URL.Host)
		require.Equal(t, http.MethodGet, r.Method)
		body := `{"object":"balance","livemode":false}`
		switch r.URL.Path {
		case "/v1/account":
			body = `{"object":"account","id":"` + account + `"}`
		case "/v1/balance":
		default:
			t.Fatalf("unexpected provider path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	construct := func(backend, snapshotID string, credentials []embed.ProviderCredentialSnapshot) *embed.Runtime {
		rt, err := embed.New(ctx, embed.Options{
			Config: &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeReadOnly, SecretBackend: backend, CredentialSnapshotID: snapshotID,
				Encryption: &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="},
				DB:         &config.DBConfig{URL: dsn}},
			PGXPool: pool, River: embed.RiverManagedByOpenRails(), StripeTransport: wire, ProviderCredentials: credentials,
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rt.Close(ctx)) })
		return rt
	}
	managed := construct(config.SecretBackendDB, "", nil)
	service := app.HostGraph(managed).Runtime.Merchants
	merchant, _, err := service.Provision(ctx, merchants.ProvisionRequest{Slug: "custody-runtime-" + uuid.NewString(), PermissionGroupID: "custody-group-" + uuid.NewString()})
	require.NoError(t, err)
	credentials := map[string]string{"secret_key": "sk_test_runtime_custody", "webhook_signing_secret": "whsec_runtime_custody"}
	zero := int64(0)
	initial, err := service.UpsertPaymentProviderConfig(ctx, merchant.ID, "stripe", merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: &zero, AccountID: account, Credentials: credentials})
	require.NoError(t, err)
	snapshotID := uuid.NewString()
	values := []embed.ProviderCredentialSnapshot{{MerchantID: merchant.ID, Rail: "stripe", AccountID: account, Credentials: credentials}}
	snapshot := construct(config.SecretBackendSnapshot, snapshotID, values)
	snapshotService := app.HostGraph(snapshot).Runtime.Merchants
	_, err = snapshotService.LoadStripeCredentials(ctx, merchant.ID)
	require.ErrorIs(t, err, merchants.ErrCredentialCustodyTransitionRequired)
	request := operator.CredentialTransitionParams{OperationID: uuid.New(), ExpectedRevision: initial.Revision, AccountID: account}
	moved, err := operator.New(snapshot).TransitionProviderCredentials(ctx, merchant.ID, "stripe", request, managed)
	require.NoError(t, err)
	require.Equal(t, initial.Revision+1, moved.Revision)
	require.Equal(t, 2, moved.Credentials["secret_key"].RotationVersion, "custody migration fences previously qualified clients")
	loaded, err := snapshotService.LoadStripeCredentials(ctx, merchant.ID)
	require.NoError(t, err)
	require.Equal(t, credentials["secret_key"], loaded.SecretKey)
	restarted := construct(config.SecretBackendSnapshot, snapshotID, values)
	loaded, err = app.HostGraph(restarted).Runtime.Merchants.LoadStripeCredentials(ctx, merchant.ID)
	require.NoError(t, err)
	require.Equal(t, credentials["webhook_signing_secret"], loaded.WebhookSigningSecret)
	replay, err := operator.New(restarted).TransitionProviderCredentials(ctx, merchant.ID, "stripe", request, managed)
	require.NoError(t, err)
	require.Equal(t, moved.Revision, replay.Revision)
	require.Equal(t, 2, replay.Credentials["secret_key"].RotationVersion)
	restored, err := operator.New(managed).TransitionProviderCredentials(ctx, merchant.ID, "stripe", operator.CredentialTransitionParams{OperationID: uuid.New(), ExpectedRevision: moved.Revision, AccountID: account}, restarted)
	require.NoError(t, err)
	require.Equal(t, moved.Revision+1, restored.Revision)
	require.Equal(t, 3, restored.Credentials["secret_key"].RotationVersion, "custody roundtrip cannot reset a credential generation")
	loaded, err = service.LoadStripeCredentials(ctx, merchant.ID)
	require.NoError(t, err)
	require.Equal(t, credentials["secret_key"], loaded.SecretKey)
}
