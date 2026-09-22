//go:build integration

package integrationharness

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authhttp"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/authkit/jwtkit"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/testauth"
	"github.com/open-rails/openrails/permissions"
	"github.com/stretchr/testify/require"
)

type delegationHTTPFixture struct {
	surface                 *Surface
	issuer                  *httptest.Server
	signer                  *jwtkit.RSASigner
	schema                  string
	claimProof              func(context.Context, string, time.Duration) (bool, error)
	application             *authkit.RemoteApplication
	email, password, access string
	subject                 string
}

func newDelegationHTTPFixture(t *testing.T, h *Harness) *delegationHTTPFixture {
	t.Helper()
	ctx := context.Background()
	surface := h.StartStandalone("usd")
	issuer := httptest.NewUnstartedServer(nil)
	t.Cleanup(issuer.Close)
	issuerURL := "http://" + issuer.Listener.Addr().String()
	schema := "delegation_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	dbtest.ApplyAuthKitMigrations(t, ctx, h.sharedPool(), schema)
	signer, err := jwtkit.NewRSASigner(2048, "browser-issuer")
	require.NoError(t, err)
	var claimProof func(context.Context, string, time.Duration) (bool, error)
	engine, err := authcore.New(authcore.Config{
		Schema: schema,
		HTTP: httpBackendBuild(func(backend authcore.HTTPBackend) (authcore.HTTPSurface, error) {
			claimProof = backend.ClaimDPoPProof
			return (authhttp.Config{DirectPeerIP: true, DisableRateLimiting: true, Mount: authhttp.MountOptions{APIPrefix: "/auth"}}).BuildHTTP(backend)
		}),
		Keys:         authcore.KeysConfig{Source: jwtkit.StaticKeySource{Active: signer, Pubs: map[string]crypto.PublicKey{signer.KID(): signer.PublicKey()}}},
		Token:        authcore.TokenConfig{Issuer: issuerURL, IssuedAudiences: []string{"merchant"}, ExpectedAudiences: []string{"merchant"}},
		Registration: authcore.RegistrationConfig{Verification: authkit.RegistrationVerificationNone},
		Delegated:    authcore.DelegatedConfig{Audiences: []string{"openrails"}, AllowDPoP: true},
	}, authcore.Deps{Postgres: h.sharedPool(), Redis: h.Redis, DelegatedAuthorization: func(context.Context, authkit.DelegationRequest) (authkit.DelegationGrant, error) {
		return authkit.DelegationGrant{Permissions: []string{permissions.MerchantAll}}, nil
	}})
	require.NoError(t, err)
	t.Cleanup(engine.Close)
	routes, err := engine.HTTPRoutes()
	require.NoError(t, err)
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Method+" "+route.Path, route.Handler)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<!doctype html><title>Merchant delegation test</title>")
	})
	issuer.Config.Handler = mux
	issuer.Start()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	email, password := "browser"+suffix+"@example.test", "Browser-proof-test-2026!"
	user, err := engine.Client().CreateUser(ctx, email, "browser"+suffix)
	require.NoError(t, err)
	require.NoError(t, engine.Client().AdminSetPassword(ctx, user.ID, password))
	response, err := issuer.Client().Post(issuer.URL+"/auth/password/login", "application/json", strings.NewReader(`{"identifier":"`+email+`","password":"`+password+`"}`))
	require.NoError(t, err)
	var session authkit.TokenSet
	require.NoError(t, json.NewDecoder(response.Body).Decode(&session))
	response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NotEmpty(t, session.AccessToken)
	cp := embcp.Get(surface.App())
	groupID := h.ensureMerchantGroup(cp.Core(), dbtest.TestMerchantSlug)
	publicDER, err := x509.MarshalPKIXPublicKey(signer.PublicKey())
	require.NoError(t, err)
	app, err := cp.Core().UpsertRemoteApplication(ctx, authkit.RemoteApplication{
		Slug: "browser" + suffix, Issuer: issuer.URL, Mode: authkit.RemoteAppModeStatic, Enabled: true, PermissionGroupID: groupID,
		PublicKeys: []authkit.RemoteAppKey{{KID: signer.KID(), PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))}},
	})
	require.NoError(t, err)
	require.NoError(t, cp.Core().OperatorAssignGroupRole(ctx, controlplane.MerchantGroup(dbtest.TestMerchantSlug), authkit.RemoteAppSubject(app.ID), controlplane.MerchantRoleOwner))
	require.NoError(t, cp.ReloadRemoteApplications(ctx))
	return &delegationHTTPFixture{surface: surface, issuer: issuer, signer: signer, schema: schema, claimProof: claimProof, application: app, email: email, password: password, access: session.AccessToken, subject: user.ID}
}

func TestDelegationHTTPWorkflow(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	f := newDelegationHTTPFixture(t, h)
	sender, err := testauth.NewSender()
	require.NoError(t, err)
	mintTarget := f.issuer.URL + "/auth/delegated/token"
	proof, err := sender.Proof(http.MethodPost, mintTarget, f.access)
	require.NoError(t, err)
	mint, err := http.NewRequest(http.MethodPost, mintTarget, strings.NewReader(`{"audiences":["openrails"],"requested_grant":{}}`))
	require.NoError(t, err)
	mint.Header.Set("Content-Type", "application/json")
	mint.Header.Set("Authorization", "Bearer "+f.access)
	mint.Header.Set("DPoP", proof)
	response, err := f.issuer.Client().Do(mint)
	require.NoError(t, err)
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	require.NoError(t, err)
	require.Equal(t, 200, response.StatusCode, string(raw))
	var token struct {
		Token     string `json:"token"`
		TokenType string `json:"token_type"`
	}
	require.NoError(t, json.Unmarshal(raw, &token))
	require.Equal(t, "DPoP", token.TokenType)
	client := &http.Client{}
	call := func(path, scheme, credential, proof string) (int, []byte, http.Header) {
		req, err := http.NewRequest(http.MethodGet, f.surface.BaseURL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", scheme+" "+credential)
		req.Header.Set("DPoP", proof)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, raw, resp.Header
	}
	fresh := func(path, credential string) string {
		p, err := sender.Proof(http.MethodGet, f.surface.BaseURL+path, credential)
		require.NoError(t, err)
		return p
	}
	const path = "/v1/me/status"
	resourceProof := fresh(path, token.Token)
	status, raw, _ := call(path, "DPoP", token.Token, resourceProof)
	require.Equal(t, 200, status, string(raw))
	status, _, headers := call(path, "DPoP", token.Token, resourceProof)
	require.Equal(t, 401, status)
	require.Contains(t, headers.Get("WWW-Authenticate"), "DPoP")
	for _, tc := range []struct{ scheme, proof string }{
		{"DPoP", ""}, {"Bearer", fresh(path, token.Token)}, {"DPoP", fresh("/other", token.Token)}, {"DPoP", fresh(path, "wrong-token")},
	} {
		status, _, _ := call(path, tc.scheme, token.Token, tc.proof)
		require.Equal(t, 401, status)
	}
	other, err := testauth.NewSender()
	require.NoError(t, err)
	wrongKey, err := other.Proof(http.MethodGet, f.surface.BaseURL+path, token.Token)
	require.NoError(t, err)
	status, _, _ = call(path, "DPoP", token.Token, wrongKey)
	require.Equal(t, 401, status)
	// Listing invoices asks the same gate for read, update, and collect authority.
	// One sender proof must survive all those checks plus the outer limiter.
	invoices := "/v1/merchant/invoices"
	status, raw, _ = call(invoices, "DPoP", token.Token, fresh(invoices, token.Token))
	require.Equal(t, 200, status, string(raw))
	credits := "/v1/merchant/customers/" + f.subject + "/credits"
	status, raw, _ = call(credits+"?currency=USD", "DPoP", token.Token, fresh(credits, token.Token))
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), `"can_grant":true`)
	require.Contains(t, string(raw), `"can_revoke":true`)
	cp := embcp.Get(f.surface.App())
	// Removing the stored application's role narrows its live grant immediately.
	require.NoError(t, cp.Core().RemoveGroupSubjectAs(ctx, h.ensureAPIKeyActor(cp, dbtest.TestMerchantSlug), controlplane.MerchantGroup(dbtest.TestMerchantSlug), authkit.RemoteAppSubject(f.application.ID)))
	status, _, _ = call(path, "DPoP", token.Token, fresh(path, token.Token))
	require.Equal(t, 401, status)
	require.NoError(t, cp.Core().OperatorAssignGroupRole(ctx, controlplane.MerchantGroup(dbtest.TestMerchantSlug), authkit.RemoteAppSubject(f.application.ID), controlplane.MerchantRoleOwner))
	f.application.Enabled = false
	_, err = cp.Core().UpsertRemoteApplication(ctx, *f.application)
	require.NoError(t, err)
	status, _, _ = call(path, "DPoP", token.Token, fresh(path, token.Token))
	require.Equal(t, 401, status)
}

// An actual TLS connection, not a proxy header or injected Request.TLS value,
// proves the native certificate profile shares the same customer route.
func TestDelegationNativeTLSWorkflow(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	f := newDelegationHTTPFixture(t, h)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "native-delegate"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	certEncoded := base64.RawURLEncoding.EncodeToString(der)
	body, err := json.Marshal(map[string]any{"audiences": []string{"openrails"}, "delegate_certificate_der_b64url": certEncoded, "requested_grant": map[string]any{}})
	require.NoError(t, err)
	mint, err := http.NewRequest(http.MethodPost, f.issuer.URL+"/auth/delegated/token", strings.NewReader(string(body)))
	require.NoError(t, err)
	mint.Header.Set("Content-Type", "application/json")
	mint.Header.Set("Authorization", "Bearer "+f.access)
	response, err := f.issuer.Client().Do(mint)
	require.NoError(t, err)
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	require.NoError(t, err)
	require.Equal(t, 200, response.StatusCode, string(raw))
	var token struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(raw, &token))
	resource := httptest.NewUnstartedServer(f.surface.Server().Handler())
	resource.TLS = &tls.Config{ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS12}
	resource.StartTLS()
	t.Cleanup(resource.Close)
	transport := resource.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.Certificates = []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}
	client := &http.Client{Transport: transport}
	req, err := http.NewRequest(http.MethodGet, resource.URL+"/v1/me/status", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token.Token)
	response, err = client.Do(req)
	require.NoError(t, err)
	raw, err = io.ReadAll(response.Body)
	response.Body.Close()
	require.NoError(t, err)
	require.Equal(t, 200, response.StatusCode, string(raw))
	req.Header.Set("X-Client-Cert", certEncoded)
	response, err = resource.Client().Do(req)
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, 401, response.StatusCode)
}

type httpBackendBuild func(authcore.HTTPBackend) (authcore.HTTPSurface, error)

func (f httpBackendBuild) BuildHTTP(b authcore.HTTPBackend) (authcore.HTTPSurface, error) {
	return f(b)
}
