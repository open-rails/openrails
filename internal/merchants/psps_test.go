package merchants

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRailDefinitions(t *testing.T) {
	// or#879/or#880: custody is not a rail and its key is not an NMI credential;
	// solana's signer is operator-only, so no merchant-visible slots.
	require.Equal(t, []billing.RailDefinition{
		{Rail: "nmi", DisplayName: "Credit Card", CredentialKeys: []string{"security_key", "webhook_signing_secret", "webhook_signing_secret_previous"}, SettingKeys: []string{"tokenization_key"}},
		{Rail: "ccbill", DisplayName: "Credit Card", CredentialKeys: []string{"salt", "datalink_username", "datalink_password"}, SettingKeys: []string{}},
		{Rail: "stripe", DisplayName: "Stripe", CredentialKeys: []string{"secret_key", "webhook_signing_secret", "webhook_signing_secret_thin", "webhook_signing_secret_previous"}, SettingKeys: []string{"publishable_key"}},
		{Rail: "solana", DisplayName: "Solana", CredentialKeys: []string{}, SettingKeys: []string{}},
	}, RailDefinitions())
}

func TestPSPView(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	configured := func(keys ...string) (out []MerchantSecretStatus) {
		for _, key := range keys {
			out = append(out, MerchantSecretStatus{Key: key, Configured: true})
		}
		return out
	}

	got := pspView(gen.BillingPsp{
		Key: "main", Rail: "stripe", Environment: "live", AccountID: "acct_123", CredentialsValidatedAt: &now,
		Settings: []byte(`{"publishable_key":"pk_live_123"}`), CredentialVersions: []byte(`{"secret_key":3}`), Revision: 4,
	}, configured("secret_key", "webhook_signing_secret"), 2)
	require.Equal(t, map[string]any{"publishable_key": "pk_live_123"}, got.Settings)
	require.Equal(t, billing.PSPCredential{Configured: true, ValidatedAt: &now, RotationVersion: 3}, got.Credentials["secret_key"])
	require.True(t, got.Credentials["webhook_signing_secret"].Configured)
	require.Nil(t, got.Credentials["webhook_signing_secret"].ValidatedAt, "format-only secrets carry no live validation")
	require.False(t, got.Credentials["webhook_signing_secret_thin"].Configured)
	require.Equal(t, int64(2), got.OpenObligations)
	require.Equal(t, int64(4), got.Revision)

	// A validation time without the checked credential still configured is
	// hidden.
	for _, statuses := range [][]MerchantSecretStatus{nil, configured("webhook_signing_secret")} {
		got := pspView(gen.BillingPsp{Rail: "nmi", Environment: "live", AccountID: "gw", CredentialsValidatedAt: &now}, statuses, 0)
		require.Nil(t, got.Credentials["security_key"].ValidatedAt)
	}
	require.NotNil(t, pspView(gen.BillingPsp{Rail: "nmi", AccountID: "gw", CredentialsValidatedAt: &now}, configured("security_key"), 0).Credentials["security_key"].ValidatedAt)

	// CCBill validation proves the DataLink pair, never the webhook salt.
	for key, want := range map[string]bool{"datalink_username": true, "datalink_password": true, "salt": false} {
		require.Equal(t, want, credentialValidatedAt("ccbill", key, &now) != nil, key)
	}
	require.Nil(t, credentialValidatedAt("solana", "private_key", &now))
}

func TestPSPSettingsAndFloors(t *testing.T) {
	// An API write overlays the stored settings; an empty value removes a key.
	require.Equal(t, map[string]any{"tokenization_key": "tk", "publishable_key": "pk"},
		mergeSettings(map[string]any{"tokenization_key": "tk", "card_entry": "browser"}, map[string]any{"publishable_key": "pk", "card_entry": ""}))
	require.NoError(t, validatePSPSettings("stripe", map[string]any{"publishable_key": "pk_test_1"}, nil))
	require.Error(t, validatePSPSettings("stripe", map[string]any{"tokenization_key": "tk"}, nil), "a setting of another rail")
	require.Error(t, validatePSPSettings("stripe", map[string]any{"publishable_key": "sk_test_1"}, nil))
	require.Error(t, validatePSPSettings("nmi", map[string]any{"tokenization_key": "same"}, map[string]string{"security_key": "same"}), "a credential stored as a setting")

	require.Equal(t, map[string]int{"secret_key": 5, "webhook_signing_secret": 2}, mergeCredentialVersions(
		map[string]int{"Secret_Key": 5, "webhook_signing_secret": 1, "bogus": 0},
		map[string]int{"secret_key": 4, "webhook_signing_secret": 2},
	))
	require.Nil(t, mergeCredentialVersions(nil, map[string]int{"x": 0}))

	state := credentialState(gen.BillingPsp{RetiredCredentials: []string{"Security_Key"}, CredentialRefs: []byte(`{"secret_key":{"name":"n","version":1}}`)})
	require.True(t, state.Retired["security_key"])
	require.Equal(t, 1, state.Refs["secret_key"].MinVersion)
}

func TestProbeNMIAndCCBillCredentials(t *testing.T) {
	nmi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, "security-key", r.Form.Get("security_key"))
		_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
	}))
	t.Cleanup(nmi.Close)
	svc := &Service{secrets: NewMemorySecretStore(), nmiCredentialProbeQueryURL: nmi.URL}
	ok, err := svc.probePaymentProviderCredentials(t.Context(), billing.MerchantID(uuid.New()), "nmi", "test", "gateway", map[string]string{"security_key": "security-key"})
	require.NoError(t, err)
	require.True(t, ok)

	// A partial CCBill update is probed as the effective (supplied + stored) pair.
	ccbill := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, "new-user", r.Form.Get("username"))
		assert.Equal(t, "stored-pass", r.Form.Get("password"))
	}))
	t.Cleanup(ccbill.Close)
	id := billing.MerchantID(uuid.New())
	store := NewMemorySecretStore()
	_, err = store.Put(t.Context(), id, "psps/ccbill/live/900000-0000/datalink_password", "stored-pass")
	require.NoError(t, err)
	svc = &Service{secrets: store, ccbillCredentialProbeBaseURL: ccbill.URL}
	ok, err = svc.probePaymentProviderCredentials(t.Context(), id, "ccbill", "live", "900000-0000", map[string]string{"datalink_username": "new-user"})
	require.NoError(t, err)
	require.True(t, ok)

	svc = &Service{secrets: NewMemorySecretStore()}
	ok, err = svc.probePaymentProviderCredentials(t.Context(), id, "ccbill", "live", "900000-0000", map[string]string{"datalink_username": "user"})
	require.ErrorContains(t, err, "required together")
	require.False(t, ok)
	ok, err = svc.probePaymentProviderCredentials(t.Context(), id, "ccbill", "live", "900000-0000", nil)
	require.NoError(t, err)
	require.False(t, ok, "nothing to probe is not a validation")
}

type stripeWire func(*http.Request) (*http.Response, error)

func (f stripeWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStripeCredentialProbeBindsAccountAndEnvironment(t *testing.T) {
	const account = `{"id":"acct_selected","object":"account"}`
	for _, tc := range []struct {
		name, key, environment, account, balance string
		status                                   int
		wireError                                bool
		ok                                       bool
		calls                                    int
	}{
		{name: "sandbox", key: "sk_test_k", environment: "test", account: account, balance: `{"object":"balance","livemode":false}`, ok: true, calls: 2},
		{name: "live restricted", key: "rk_live_k", environment: "live", account: account, balance: `{"object":"balance","livemode":true}`, ok: true, calls: 2},
		{name: "foreign account", key: "sk_test_k", environment: "test", account: `{"id":"acct_other","object":"account"}`, calls: 1},
		{name: "wrong object", key: "sk_test_k", environment: "test", account: `{"id":"acct_selected","object":"customer"}`, calls: 1},
		{name: "missing account id", key: "sk_test_k", environment: "test", account: `{"object":"account"}`, calls: 1},
		{name: "missing account object", key: "sk_test_k", environment: "test", account: `{"id":"acct_selected"}`, calls: 1},
		{name: "wrong response environment", key: "sk_test_k", environment: "test", account: account, balance: `{"object":"balance","livemode":true}`, calls: 2},
		{name: "missing livemode", key: "sk_test_k", environment: "test", account: account, balance: `{"object":"balance"}`, calls: 2},
		{name: "null livemode", key: "sk_test_k", environment: "test", account: account, balance: `{"object":"balance","livemode":null}`, calls: 2},
		{name: "missing balance object", key: "sk_test_k", environment: "test", account: account, balance: `{"livemode":false}`, calls: 2},
		{name: "wrong key environment", key: "sk_live_k", environment: "test"},
		{name: "unknown environment", key: "sk_test_k", environment: "unknown"},
		{name: "malformed", key: "sk_test_k", environment: "test", account: `{"id":`, calls: 1},
		{name: "oversized", key: "sk_test_k", environment: "test", account: strings.Repeat("x", (1<<20)+1), calls: 1},
		{name: "denied", key: "sk_test_k", environment: "test", status: 403, calls: 1},
		{name: "redirect", key: "sk_test_k", environment: "test", status: 302, calls: 1},
		{name: "unavailable", key: "sk_test_k", environment: "test", wireError: true, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			svc := &Service{StripeClients: stripeapi.NewFactory(stripeWire(func(r *http.Request) (*http.Response, error) {
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
					return nil, errors.New("unexpected request")
				}
				status := tc.status
				if status == 0 {
					status = 200
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://unexpected.example/secret"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))}
			ok, err := svc.probePaymentProviderCredentials(context.Background(), billing.MerchantID(uuid.New()), "stripe", tc.environment, "acct_selected", map[string]string{"secret_key": tc.key})
			require.Equal(t, tc.ok, ok)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), tc.key, "probe errors never echo the credential")
			}
			require.Equal(t, tc.calls, calls)
		})
	}
}

// #850: api_host is operator-supplied; normalize then validate.
func TestAPIHostValidation(t *testing.T) {
	for _, host := range []string{"api.myapp.example", "api.host-late.test", "localhost", "a1.b2.c3"} {
		require.NoError(t, ValidateAPIHost(host), host)
	}
	for _, host := range []string{"", "https://api.myapp.example", "api.myapp.example/path", "api my app", "API.Upper.Case", ".leading.dot", "double..dot", "-leading.hyphen", "trailing-.hyphen", "under_score.example", strings.Repeat("a", 64) + ".example"} {
		require.ErrorIs(t, ValidateAPIHost(host), ErrInvalidAPIHost, host)
	}
	require.Equal(t, "api.myapp.example", NormalizeAPIHost("  API.MyApp.Example:8443 "))
	require.Empty(t, NormalizeAPIHost("   "))
	require.NoError(t, ValidateAPIHost(NormalizeAPIHost("API.MyApp.Example:8443")))
}
