//go:build integration

package embed_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/httptesthost"
)

type noConfigurationProviderRequests struct{}

func (noConfigurationProviderRequests) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected provider request in metadata-only workflow")
}

func TestMerchantConfigurationPublicationBoundary(t *testing.T) {
	for _, backend := range []string{config.SecretBackendSnapshot, config.SecretBackendDB} {
		for _, publish := range []bool{false, true} {
			t.Run(backend+"/http-"+strconv.FormatBool(publish), func(t *testing.T) {
				ctx := t.Context()
				cfg := &config.Config{
					DB:       &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)},
					TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeReadOnly,
					SecretBackend: backend,
				}
				if backend == config.SecretBackendDB {
					cfg.Encryption = &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
				}
				slug := "config-boundary-" + uuid.NewString()
				runtime, id, err := newDeclaredMerchant(ctx, embed.Options{
					Config: cfg, River: embed.RiverManagedByOpenRails(), StripeTransport: noConfigurationProviderRequests{},
				}, slug, embed.MerchantConfig{DisplayName: "Original"})
				require.NoError(t, err)
				t.Cleanup(func() { _ = runtime.Close(context.Background()) })
				local, err := runtime.Client()
				require.NoError(t, err)
				before, err := local.MerchantConfiguration.Retrieve(ctx, openrails.WithMerchant(slug))
				require.NoError(t, err)
				name := "Changed through the local Client"
				params := &openrails.MerchantConfigurationApplyParams{
					ApplicationID: uuid.NewString(), ExpectedRevision: &before.Revision, DisplayName: &name,
				}
				receipt, err := local.MerchantConfiguration.Apply(ctx, params, openrails.WithMerchant(slug))
				require.NoError(t, err)
				require.False(t, receipt.Replayed)
				replayed, err := local.MerchantConfiguration.Apply(ctx, params)
				require.NoError(t, err)
				require.True(t, replayed.Replayed)

				zero := int64(0)
				credentials := &openrails.UpsertPaymentProviderParams{
					OperationID: uuid.New(), ExpectedRevision: &zero,
					AccountID:   "acct_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
					Credentials: map[string]string{"webhook_signing_secret": "whsec_boundary_fixture"},
				}
				_, err = local.PaymentProviders.Upsert(ctx, "stripe", credentials)
				if backend == config.SecretBackendSnapshot {
					var status *openrails.StatusError
					require.ErrorAs(t, err, &status)
					require.Equal(t, http.StatusMethodNotAllowed, status.Status)
					require.Equal(t, "credential_source_read_only", status.Code)
				} else {
					require.NoError(t, err, "writable local credentials do not require public HTTP")
				}

				handler, err := httptesthost.Handler(runtime, httptesthost.Options{
					HTTP: embed.HTTPConfig{MerchantConfig: publish}, Gate: allowAllGate{id: id},
				})
				require.NoError(t, err)
				server := httptest.NewServer(handler)
				t.Cleanup(server.Close)
				remote, err := openrails.NewRemote(server.URL, openrails.WithAPIKey("fixture"), openrails.WithMerchantID(id))
				require.NoError(t, err)
				state, err := remote.MerchantConfiguration.Retrieve(ctx)
				if !publish {
					var status *openrails.StatusError
					require.ErrorAs(t, err, &status)
					require.Equal(t, http.StatusNotFound, status.Status)
					_, err = remote.PaymentProviders.List(ctx, nil)
					require.ErrorAs(t, err, &status)
					require.Equal(t, http.StatusNotFound, status.Status)
					return
				}
				require.NoError(t, err)
				require.Equal(t, name, state.DisplayName)
				require.Equal(t, receipt.Revision, state.Revision)
				remoteReplay, err := remote.MerchantConfiguration.Apply(ctx, params)
				require.NoError(t, err)
				require.True(t, remoteReplay.Replayed)
				providers, err := remote.PaymentProviders.List(ctx, nil)
				require.NoError(t, err)
				if backend == config.SecretBackendDB {
					require.Len(t, providers.Data, 1)
					require.True(t, providers.Data[0].Credentials["webhook_signing_secret"].Configured)
				}
			})
		}
	}
}
