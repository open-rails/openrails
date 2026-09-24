package merchants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPaymentProviderDefinitions(t *testing.T) {
	// or#879/or#880: custody is not a rail and its key is not an NMI credential;
	// solana's signer is operator-only, so no merchant-visible slots.
	require.Equal(t, []PaymentProviderDefinition{
		{Rail: "nmi", DisplayName: "Credit Card", CredentialKeys: []string{"security_key", "webhook_signing_secret", "webhook_signing_secret_previous"}},
		{Rail: "ccbill", DisplayName: "Credit Card", CredentialKeys: []string{"salt", "datalink_username", "datalink_password"}},
		{Rail: "stripe", DisplayName: "Stripe", CredentialKeys: []string{"secret_key", "webhook_signing_secret", "webhook_signing_secret_thin", "webhook_signing_secret_previous"}},
		{Rail: "solana", DisplayName: "Solana", CredentialKeys: []string{}},
	}, PaymentProviderDefinitions())
}

func TestPaymentProviderConfigProjection(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	name := func(rail, account, key string) string {
		n, err := PSPSecretName(rail, "live", account, key)
		require.NoError(t, err)
		return n
	}
	configured := func(names ...string) (out []MerchantSecretStatus) {
		for _, n := range names {
			out = append(out, MerchantSecretStatus{Name: n, Configured: true})
		}
		return out
	}

	got := paymentProviderConfigFromRow(gen.OpenrailsPsp{
		Rail: "stripe", Environment: "live", AccountID: "acct_123", LastVerifiedAt: &now,
		Evidence: []byte(`{"public_config":{"publishable_key":"pk_live_123"},"credentials_validated":true,"credential_versions":{"secret_key":3}}`),
	}, configured(name("stripe", "acct_123", "secret_key"), name("stripe", "acct_123", "webhook_signing_secret")))
	require.Equal(t, map[string]string{"publishable_key": "pk_live_123"}, got.PublicConfig)
	require.Equal(t, &now, got.LastVerifiedAt)
	require.Equal(t, PaymentProviderCredentialStatus{Configured: true, LastValidatedAt: &now, RotationVersion: 3}, got.Credentials["secret_key"])
	require.True(t, got.Credentials["webhook_signing_secret"].Configured)
	require.Nil(t, got.Credentials["webhook_signing_secret"].LastValidatedAt, "format-only secrets carry no live validation")
	require.False(t, got.Credentials["webhook_signing_secret_thin"].Configured)

	// A verification timestamp without probe evidence, or without the probed
	// credential still configured, is hidden.
	for _, tc := range []struct {
		evidence string
		statuses []MerchantSecretStatus
	}{
		{``, configured(name("nmi", "gw", "security_key"))},
		{`{"credentials_validated":true}`, nil},
		{`{"credentials_validated":true}`, configured(name("nmi", "gw", "webhook_signing_secret"))},
	} {
		got := paymentProviderConfigFromRow(gen.OpenrailsPsp{Rail: "nmi", Environment: "live", AccountID: "gw", LastVerifiedAt: &now, Evidence: []byte(tc.evidence)}, tc.statuses)
		require.Nil(t, got.LastVerifiedAt, tc.evidence)
		require.Nil(t, got.Credentials["security_key"].LastValidatedAt, tc.evidence)
	}
	retired := paymentProviderConfigFromRow(gen.OpenrailsPsp{Rail: "nmi", Environment: "live", AccountID: "gw", Evidence: []byte(`{"retired_credentials":{"security_key":true}}`)}, configured(name("nmi", "gw", "security_key")))
	require.False(t, retired.Credentials["security_key"].Configured, "a retired credential is never shown as configured")

	// CCBill validation proves the DataLink pair, never the webhook salt.
	for key, want := range map[string]bool{"datalink_username": true, "datalink_password": true, "salt": false} {
		require.Equal(t, want, credentialValidatedAt("ccbill", key, &now) != nil, key)
	}
	require.Nil(t, credentialValidatedAt("solana", "private_key", &now))
}

func TestProviderEvidenceMergesAndFloorsNeverRegress(t *testing.T) {
	// The manifest also writes evidence; an API write must merge, not replace.
	out, err := marshalProviderEvidence([]byte(`{"settings":{"tokenization_key":"tk"},"source":"manifest","credentials_validated":true}`), map[string]string{"publishable_key": "pk"}, false, map[string]int{"secret_key": 2})
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))
	require.Equal(t, map[string]any{
		"settings": map[string]any{"tokenization_key": "tk"}, "source": "manifest",
		"public_config": map[string]any{"publishable_key": "pk"}, "credential_versions": map[string]any{"secret_key": float64(2)},
	}, doc)
	out, err = marshalProviderEvidence(nil, nil, false, nil)
	require.NoError(t, err)
	require.Nil(t, out)

	require.Equal(t, map[string]int{"secret_key": 5, "webhook_signing_secret": 2}, mergeCredentialVersions(
		map[string]int{"Secret_Key": 5, "webhook_signing_secret": 1, "bogus": 0},
		map[string]int{"secret_key": 4, "webhook_signing_secret": 2},
	))
	require.Nil(t, mergeCredentialVersions(nil, map[string]int{"x": 0}))

	// Declared settings win; API public_config only fills gaps.
	require.Equal(t, map[string]any{"tokenization_key": "declared", "publishable_key": "pk_test_api"},
		pspSettings([]byte(`{"settings":{"tokenization_key":"declared"},"public_config":{"tokenization_key":"api","publishable_key":"pk_test_api"}}`)))
	require.Nil(t, pspSettings([]byte(`{"source":"x"}`)))
}

func TestProbeNMIAndCCBillCredentials(t *testing.T) {
	nmi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, "security-key", r.Form.Get("security_key"))
		_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
	}))
	t.Cleanup(nmi.Close)
	svc := &Service{secrets: NewMemorySecretStore(), nmiCredentialProbeQueryURL: nmi.URL}
	ok, err := svc.probePaymentProviderCredentials(t.Context(), merchant.ID(uuid.New()), "nmi", "test", "gateway", map[string]string{"security_key": "security-key"})
	require.NoError(t, err)
	require.True(t, ok)

	// A partial CCBill update is probed as the effective (supplied + stored) pair.
	ccbill := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, "new-user", r.Form.Get("username"))
		assert.Equal(t, "stored-pass", r.Form.Get("password"))
	}))
	t.Cleanup(ccbill.Close)
	id := merchant.ID(uuid.New())
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
			ok, err := svc.probePaymentProviderCredentials(context.Background(), merchant.ID(uuid.New()), "stripe", tc.environment, "acct_selected", map[string]string{"secret_key": tc.key})
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
