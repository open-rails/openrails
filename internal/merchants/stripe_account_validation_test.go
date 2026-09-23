package merchants

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type stripeCredentialWire func(*http.Request) (*http.Response, error)

func (f stripeCredentialWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStripeCredentialProbeBindsAccountAndEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, key, environment, account, balance string
		status                                   int
		wireError                                bool
		wantErr                                  bool
		calls                                    int
	}{
		{name: "sandbox", key: "sk_test_candidate", environment: "test", account: `{"id":"acct_selected","object":"account"}`, balance: `{"object":"balance","livemode":false}`, calls: 2},
		{name: "live restricted", key: "rk_live_candidate", environment: "live", account: `{"id":"acct_selected","object":"account"}`, balance: `{"object":"balance","livemode":true}`, calls: 2},
		{name: "foreign account", key: "sk_test_candidate", environment: "test", account: `{"id":"acct_other","object":"account"}`, wantErr: true, calls: 1},
		{name: "wrong object", key: "sk_test_candidate", environment: "test", account: `{"id":"acct_selected","object":"customer"}`, wantErr: true, calls: 1},
		{name: "wrong response environment", key: "sk_test_candidate", environment: "test", account: `{"id":"acct_selected","object":"account"}`, balance: `{"object":"balance","livemode":true}`, wantErr: true, calls: 2},
		{name: "missing response environment", key: "sk_test_candidate", environment: "test", account: `{"id":"acct_selected","object":"account"}`, balance: `{"object":"balance"}`, wantErr: true, calls: 2},
		{name: "missing account id", key: "sk_test_candidate", environment: "test", account: `{"object":"account"}`, wantErr: true, calls: 1},
		{name: "missing account object", key: "sk_test_candidate", environment: "test", account: `{"id":"acct_selected"}`, wantErr: true, calls: 1},
		{name: "null balance environment", key: "sk_test_candidate", environment: "test", account: `{"id":"acct_selected","object":"account"}`, balance: `{"object":"balance","livemode":null}`, wantErr: true, calls: 2},
		{name: "missing balance object", key: "sk_test_candidate", environment: "test", account: `{"id":"acct_selected","object":"account"}`, balance: `{"livemode":false}`, wantErr: true, calls: 2},
		{name: "wrong key environment", key: "sk_live_candidate", environment: "test", wantErr: true},
		{name: "unknown environment", key: "sk_test_candidate", environment: "unknown", wantErr: true},
		{name: "malformed", key: "sk_test_candidate", environment: "test", account: `{"id":`, wantErr: true, calls: 1},
		{name: "oversized", key: "sk_test_candidate", environment: "test", account: strings.Repeat("x", (1<<20)+1), wantErr: true, calls: 1},
		{name: "denied", key: "sk_test_candidate", environment: "test", status: 403, wantErr: true, calls: 1},
		{name: "redirect", key: "sk_test_candidate", environment: "test", status: 302, wantErr: true, calls: 1},
		{name: "unavailable", key: "sk_test_candidate", environment: "test", wireError: true, wantErr: true, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			service := &Service{StripeClients: stripeapi.NewFactory(stripeCredentialWire(func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "api.stripe.com", r.URL.Host)
				require.Equal(t, stripeapi.APIVersion, r.Header.Get(stripeapi.VersionHeader))
				require.Equal(t, "Bearer "+tc.key, r.Header.Get("Authorization"))
				require.Empty(t, r.Header.Get("Stripe-Account"))
				if tc.wireError {
					return nil, fmt.Errorf("transport echoed secret %s", tc.key)
				}
				body := tc.account
				switch r.URL.Path {
				case "/v1/account":
				case "/v1/balance":
					body = tc.balance
				default:
					return nil, errors.New("unexpected test request")
				}
				status := tc.status
				if status == 0 {
					status = 200
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": []string{"https://unexpected.example/secret"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))}
			verified, err := service.probePaymentProviderCredentials(context.Background(), merchant.ID(uuid.New()), "stripe", tc.environment, "acct_selected", map[string]string{"secret_key": tc.key})
			if tc.wantErr {
				require.Error(t, err)
				require.False(t, verified)
				require.NotContains(t, err.Error(), tc.key)
			} else {
				require.NoError(t, err)
				require.True(t, verified)
			}
			require.Equal(t, tc.calls, calls)
		})
	}
}
