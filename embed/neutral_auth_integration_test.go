//go:build integration

package embed_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	auth "github.com/open-rails/helpers/auth"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

// This provider depends only on the neutral protocol. It owns credential
// verification and live permissions, with no AuthKit types or permission roles.
type independentVerifier struct {
	secret  []byte
	allowed atomic.Bool
	calls   atomic.Int64
}
type independentPrincipal struct {
	identity auth.Identity
	provider *independentVerifier
}

var _ billingauth.Verifier = (*independentVerifier)(nil)
var _ auth.Principal = independentPrincipal{}
var _ auth.PermissionChecker = independentPrincipal{}

func (p independentPrincipal) Identity() auth.Identity { return p.identity }
func (p independentPrincipal) Can(_ context.Context, scope auth.Scope, permission string) (bool, error) {
	return p.provider.allowed.Load() && (p.identity.Subject == "staff" || p.identity.Subject == "machine") && scope.Authority == "https://independent.test" && scope.ID == "billing-staff" && permission == permissions.MerchantCustomerSettingsRead, nil
}
func (v *independentVerifier) AuthenticateRequest(_ context.Context, r *http.Request) (auth.Principal, error) {
	v.calls.Add(1)
	token, err := jwt.Parse(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), func(*jwt.Token) (any, error) { return v.secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("https://independent.test"), jwt.WithAudience("billing"), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		return nil, auth.ErrUnauthenticated
	}
	subject, err := token.Claims.GetSubject()
	if err != nil || subject == "" {
		return nil, auth.ErrUnauthenticated
	}
	if subject == "policy-denied" {
		return nil, auth.ErrForbidden
	}
	kind := auth.KindUser
	if subject == "machine" {
		kind = auth.KindAPIKey
	}
	return independentPrincipal{identity: auth.Identity{Kind: kind, Issuer: "https://independent.test", Subject: subject}, provider: v}, nil
}

func TestIndependentProviderEmbeddedCustomerAndStaffRoutes(t *testing.T) {
	ctx := t.Context()
	_, pool, dsn := scopeWithoutRLSDatabase(t)
	customer := uuid.NewString()
	slug := "independent-" + uuid.NewString()
	verifier := &independentVerifier{secret: []byte("independent-provider-signed-test-credential")}
	integration, err := billingauth.NewIntegration(billingauth.IntegrationOptions{
		Verifier: verifier,
		Customer: func(_ context.Context, p auth.Principal) (billingauth.CustomerIdentity, error) {
			if p.Identity().Subject == "customer" {
				return billingauth.CustomerIdentity{ID: customer, CredentialClass: billingauth.CredentialClassUserSession}, nil
			}
			return billingauth.CustomerIdentity{}, nil
		},
		Authority: func(_ context.Context, q billingauth.Requirement) (billingauth.Authority, error) {
			if q.Scope != billingauth.MerchantScope || q.Target.MerchantSlug != slug || q.Target.MerchantID.IsZero() {
				return billingauth.Authority{}, nil
			}
			return billingauth.Authority{Scope: auth.Scope{Authority: "https://independent.test", ID: "billing-staff"}, Permission: q.Permission}, nil
		},
	})
	require.NoError(t, err)
	runtime, _, err := newDeclaredMerchant(ctx, embed.Options{
		Config:  &config.Config{Env: "development", TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeReadOnly, DB: &config.DBConfig{URL: dsn}},
		PGXPool: pool, River: embed.RiverFromHost(), Auth: integration,
		HTTP: &embed.HTTPConfig{MerchantAdmin: true, CustomerRoutes: []embed.CustomerRoutesConfig{{Merchant: slug, Treasury: true}}},
	}, slug, embed.MerchantConfig{DisplayName: "Independent identity provider"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	bundle, err := openrailshttp.Routes(runtime)
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, bundle.Mount(mux, "/billing"))
	issue := func(subject, audience string) string {
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": subject, "iss": "https://independent.test", "aud": audience, "exp": time.Now().Add(time.Hour).Unix(), "roles": []string{"root", "owner"}}).SignedString(verifier.secret)
		require.NoError(t, err)
		return token
	}
	customerToken, staffToken := issue("customer", "billing"), issue("staff", "billing")
	request := func(path, token string, status int) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/billing"+path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		before := verifier.calls.Load()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, r)
		require.Equal(t, status, response.Code, response.Body.String())
		require.Equal(t, before+1, verifier.calls.Load(), "one verification for authentication and any live authorization")
	}
	own := "/v1/customers/" + customer + "/balance?currency=USD"
	request(own, customerToken, http.StatusOK)
	request(own, staffToken, http.StatusUnauthorized)
	request(own, customerToken+"tampered", http.StatusUnauthorized)
	request(own, issue("policy-denied", "billing"), http.StatusForbidden)
	request(own, issue("customer", "another-service"), http.StatusUnauthorized)
	staff := "/v1/merchant/customers"
	request(staff, staffToken, http.StatusForbidden)
	verifier.allowed.Store(true)
	request(staff, staffToken, http.StatusOK)
	request(staff, issue("machine", "billing"), http.StatusOK)
	request(staff, issue("policy-denied", "billing"), http.StatusForbidden)
	request(staff, customerToken, http.StatusForbidden)
	verifier.allowed.Store(false)
	request(staff, staffToken, http.StatusForbidden)
	for _, path := range []string{"/v1/merchant/team", "/v1/merchant/api-keys"} {
		r := httptest.NewRequest(http.MethodGet, "/billing"+path, nil)
		r.Header.Set("Authorization", "Bearer "+issue("machine", "billing"))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, r)
		require.Equal(t, http.StatusNotFound, response.Code, "identity administration is host-owned")
	}
}
