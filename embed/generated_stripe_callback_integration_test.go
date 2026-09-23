//go:build integration

package embed_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestGeneratedStripeCallbackNativeMounts(t *testing.T) {
	ctx := t.Context()
	var providerRequests int
	refuseProvider := callbackRejectTransport(func(*http.Request) (*http.Response, error) {
		providerRequests++
		return nil, fmt.Errorf("no provider calls permitted")
	})
	runtime, err := embed.New(ctx, embed.Options{HTTP: &embed.HTTPConfig{}, StripeTransport: refuseProvider, Config: &config.Config{
		TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeReadOnly,
		SecretBackend: config.SecretBackendSnapshot, DB: &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)},
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	rt := app.HostGraph(runtime).Runtime
	admin := dbtest.SharedSuperuserPGXPool(t)
	seed := func(environment string) (merchant.ID, string, string) {
		id, account, secret := merchant.ID(uuid.New()), "acct_"+uuid.NewString(), "whsec_"+uuid.NewString()
		_, err := admin.Exec(ctx, "INSERT INTO billing.merchants(id,slug) VALUES($1,$2)", id.UUID(), "callback-"+id.String())
		require.NoError(t, err)
		_, err = admin.Exec(ctx, "INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id,key) VALUES($1,$2,'stripe',$3,$4,$4)", uuid.New(), id.UUID(), environment, account)
		require.NoError(t, err)
		for key, value := range map[string]string{"secret_key": "sk_test_fixture", "webhook_signing_secret": secret} {
			name, err := merchants.PSPSecretName("stripe", environment, account, key)
			require.NoError(t, err)
			_, err = rt.ManifestSecrets.Seeder().Put(ctx, id, name, value)
			require.NoError(t, err)
		}
		return id, account, secret
	}
	selected, account, secret := seed("test")
	_, foreignAccount, foreignSecret := seed("test")
	_, liveAccount, liveSecret := seed("live")
	bundle, err := openrailshttp.Routes(runtime)
	require.NoError(t, err)
	for _, prefix := range []string{"", "/billing"} {
		for _, bound := range []bool{false, true} {
			t.Run(fmt.Sprintf("prefix=%s/bound=%t", prefix, bound), func(t *testing.T) {
				rt.SetConfiguredMerchant(merchant.ID{})
				if bound {
					rt.SetConfiguredMerchant(selected)
				}
				mux := http.NewServeMux()
				require.NoError(t, bundle.Mount(mux, prefix))
				cfg := &config.Config{PublicBillingBaseURL: "https://billing.example.com" + prefix}
				post := func(accountID, signingSecret, payloadAccount string) int {
					t.Helper()
					generated, ok, err := catalog.PublicStripeWebhookURL(cfg, accountID)
					require.NoError(t, err)
					require.True(t, ok)
					u, err := url.Parse(generated)
					require.NoError(t, err)
					eventID := "evt_" + uuid.NewString()
					body := []byte(fmt.Sprintf(`{"id":%q,"type":"customer.created","account":%q,"data":{"object":{"id":"cus_fixture","object":"customer"}}}`, eventID, payloadAccount))
					now := time.Now().Unix()
					mac := hmac.New(sha256.New, []byte(signingSecret))
					fmt.Fprintf(mac, "%d.%s", now, body)
					request := httptest.NewRequest(http.MethodPost, u.RequestURI(), bytes.NewReader(body))
					request.Host = "api.unrecognized-host.test"
					request.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%x", now, mac.Sum(nil)))
					response := httptest.NewRecorder()
					mux.ServeHTTP(response, request)
					if payloadAccount != accountID {
						require.Contains(t, response.Body.String(), `"code":"webhook_account_mismatch"`)
						require.Contains(t, response.Body.String(), "Webhook account does not match payload")
						var stored int
						require.NoError(t, admin.QueryRow(ctx, "SELECT count(*) FROM billing.webhook_events WHERE event_id=$1", eventID).Scan(&stored))
						require.Zero(t, stored, "scope mismatch must not reach durable webhook acceptance")
						require.Zero(t, providerRequests, "scope check runs before any provider hydration request")
					}

					return response.Code
				}
				require.Equal(t, http.StatusOK, post(account, secret, account))
				require.Equal(t, http.StatusUnauthorized, post(account, "whsec_wrong", account))
				require.Equal(t, http.StatusBadRequest, post(account, secret, foreignAccount))
				require.Equal(t, http.StatusNotFound, post(liveAccount, liveSecret, liveAccount))
				want := http.StatusOK
				if bound {
					want = http.StatusNotFound
				}
				require.Equal(t, want, post(foreignAccount, foreignSecret, foreignAccount))
			})
		}
	}
}

type callbackRejectTransport func(*http.Request) (*http.Response, error)

func (f callbackRejectTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
