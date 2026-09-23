//go:build integration

package embed_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// Constructor policy needs no merchant UUID or late-bound delegated bridge.
// Native self ownership uses the issuer-aware canonical customer mapping;
// another payer requires live permission for the exact resolved operation.
func TestNativeTreasuryConstructorAuthority(t *testing.T) {
	ctx := t.Context()
	_, pool, dsn := scopeWithoutRLSDatabase(t)
	slug := "native-treasury-" + uuid.NewString()
	customers := map[string]string{"https://issuer-a.test": uuid.NewString(), "https://issuer-b.test": uuid.NewString()}
	secret := []byte("native-treasury-constructor-fixture-key")
	issue := func(issuer, kind string) string {
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": "same-opaque-subject", "iss": issuer, "aud": "billing", "kind": kind,
			"exp": time.Now().Add(time.Hour).Unix(), "permissions": []string{"*"},
		}).SignedString(secret)
		require.NoError(t, err)
		return token
	}
	nativeA, nativeB := issue("https://issuer-a.test", "user"), issue("https://issuer-b.test", "user")
	machine := issue("https://issuer-a.test", "machine")
	var grantMerchant, authorityUnavailable atomic.Bool
	var authorizationCalls atomic.Int64
	var requirementMu sync.Mutex
	var last billingauth.Requirement
	integration := &billingauth.Integration{
		Authentication: billingauth.AuthenticationFunc(func(_ context.Context, r *http.Request) (billingauth.Identity, error) {
			raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			parsed, err := jwt.Parse(raw, func(*jwt.Token) (any, error) { return secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience("billing"))
			if err != nil || !parsed.Valid {
				return billingauth.Identity{}, billingauth.ErrUnauthenticated
			}
			claims := parsed.Claims.(jwt.MapClaims)
			issuer, _ := claims.GetIssuer()
			customer, ok := customers[issuer]
			if !ok {
				return billingauth.Identity{}, billingauth.ErrUnauthenticated
			}
			subject, _ := claims.GetSubject()
			identity := billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: subject, CustomerID: customer, Issuer: issuer, CredentialClass: billingauth.CredentialClassUserSession}
			if claims["kind"] == "machine" {
				identity.Kind, identity.CredentialClass = billingauth.Machine, "machine"
				identity.Permissions = []string{permissions.CustomerAll, permissions.MerchantAll}
			}
			return identity, nil
		}),
		Authorization: billingauth.AuthorizationFunc(func(_ context.Context, _ *http.Request, _ billingauth.Identity, q billingauth.Requirement) error {
			authorizationCalls.Add(1)
			requirementMu.Lock()
			last = q
			requirementMu.Unlock()
			if authorityUnavailable.Load() {
				return errors.New("fixture authority backend unavailable")
			}
			if grantMerchant.Load() && q.Scope == billingauth.CustomerScope && q.Permission == permissions.CustomerBalanceRead && q.Target.CustomerID == q.Target.MerchantID.String() {
				return nil
			}
			return billingauth.GateError{Status: http.StatusForbidden, Message: "payer permission denied"}
		}),
	}
	runtime, mid, err := newDeclaredMerchant(ctx, embed.Options{
		Config: &config.Config{
			Env: "development", TestMode: config.CredentialPostureSandbox,
			MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB,
			ProviderWriteMode: config.ProviderWriteModeReadOnly, DB: &config.DBConfig{URL: dsn},
		},
		PGXPool: pool, River: embed.RiverFromHost(), Auth: integration,
		HTTP: &embed.HTTPConfig{CustomerRoutes: []embed.CustomerRoutesConfig{{Merchant: slug, Treasury: true}}},
	}, slug, embed.MerchantConfig{DisplayName: "Native treasury"})
	require.NoError(t, err, "native treasury is complete constructor policy without a UUID-dependent callback")
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	routes, err := runtime.HTTPRoutes()
	require.NoError(t, err)
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Method+" "+route.Path, route.Handler)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	balance := func(token, payer string) int {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/customers/"+payer+"/balance?currency=USD", nil)
		require.NoError(t, err)
		r.Header.Set("Authorization", "Bearer "+token)
		response, err := server.Client().Do(r)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode
	}
	for issuer, token := range map[string]string{"https://issuer-a.test": nativeA, "https://issuer-b.test": nativeB} {
		require.Equal(t, http.StatusOK, balance(token, customers[issuer]))
	}
	require.Equal(t, http.StatusForbidden, balance(nativeA, customers["https://issuer-b.test"]), "same opaque subject from another issuer is not the same payable customer")
	for _, payer := range []string{slug, mid.String()} {
		require.Equal(t, http.StatusForbidden, balance(nativeA, payer), "known merchant coordinates grant no treasury authority")
	}
	grantMerchant.Store(true)
	for _, payer := range []string{slug, mid.String()} {
		before := authorizationCalls.Load()
		require.Equal(t, http.StatusOK, balance(nativeA, payer))
		require.Equal(t, before+1, authorizationCalls.Load(), "privileged payer permission is checked once for this request")
		requirementMu.Lock()
		observed := last
		requirementMu.Unlock()
		require.Equal(t, billingauth.CustomerScope, observed.Scope)
		require.Equal(t, permissions.CustomerBalanceRead, observed.Permission)
		require.Equal(t, mid, observed.Target.MerchantID)
		require.Equal(t, mid.String(), observed.Target.CustomerID)
	}
	require.Equal(t, http.StatusForbidden, balance(nativeA, customers["https://issuer-b.test"]), "merchant payer grant is not blanket sibling access")
	grantMerchant.Store(false)
	require.Equal(t, http.StatusForbidden, balance(nativeA, slug), "revocation applies to the same still-valid session")
	authorityUnavailable.Store(true)
	require.Equal(t, http.StatusServiceUnavailable, balance(nativeA, slug))
	authorityUnavailable.Store(false)
	grantMerchant.Store(true)
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, balance(machine, slug), "machine credentials cannot become native payer sessions")
	require.Equal(t, http.StatusUnauthorized, balance(nativeA+"tampered", customers["https://issuer-a.test"]))
}
