package embedhttp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	coreauth "github.com/open-rails/authkit"
	"github.com/open-rails/authkit/jwtkit"
	"github.com/open-rails/authkit/verify"
	auth "github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Static authority records isolate the crypto/proof boundary.
type proofAuthority struct {
	coreauth.Client
	application coreauth.RemoteApplication
	group       coreauth.GroupInstance
}

func (s *proofAuthority) ListEnabledRemoteApplications(context.Context) ([]coreauth.RemoteApplication, error) {
	return []coreauth.RemoteApplication{s.application}, nil
}
func (s *proofAuthority) GetRemoteApplication(context.Context, string) (*coreauth.RemoteApplication, error) {
	return &s.application, nil
}
func (s *proofAuthority) ResolveRemoteApplicationAuthority(context.Context, string) (coreauth.RemoteApplicationAuthority, error) {
	return coreauth.RemoteApplicationAuthority{Permissions: []string{permissions.MerchantCatalogRead}, PermissionGroupID: s.group.ID, AuthorityIssuer: "https://authority.test", Persona: "merchant", InstanceSlug: "store"}, nil
}
func (s *proofAuthority) ResolveAPIKeyDetailed(context.Context, string, string) (coreauth.ResolvedAPIKey, error) {
	return coreauth.ResolvedAPIKey{}, errors.New("no API key")
}

// GroupInstanceByID reports the bound group live (AuthKit re-checks scoped
// machine permissions against it).
func (s *proofAuthority) GroupInstanceByID(_ context.Context, id string) (coreauth.GroupInstance, error) {
	if id != s.group.ID {
		return coreauth.GroupInstance{}, coreauth.ErrGroupNotFound
	}
	return s.group, nil
}

func (s *proofAuthority) GroupInstanceForSlug(context.Context, coreauth.GroupRef) (coreauth.GroupInstance, error) {
	return s.group, nil
}

type proofDirectory struct{ row merchants.Merchant }

func (d proofDirectory) Get(context.Context, merchant.ID) (*merchants.Merchant, error) {
	row := d.row
	return &row, nil
}
func (d proofDirectory) GetBySlug(context.Context, string) (*merchants.Merchant, error) {
	row := d.row
	return &row, nil
}
func (d proofDirectory) HasCanonicalNameAuthority() bool { return true }
func (d proofDirectory) CanonicalSlug(context.Context, merchant.ID) (string, error) {
	return d.row.Slug, nil
}

func TestDPoPProofVerifiedOnceAcrossV2RouteAndAuthorization(t *testing.T) {
	const issuer = "https://delegating-app.test"
	const origin = "https://billing.test"
	signer, err := jwtkit.NewRSASigner(2048, "proof-issuer")
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(signer.PublicKey())
	require.NoError(t, err)
	groupID := uuid.NewString()
	authority := &proofAuthority{application: coreauth.RemoteApplication{ID: uuid.NewString(), Slug: "delegator", Issuer: issuer, Enabled: true, Mode: coreauth.RemoteAppModeStatic, PublicKeys: []coreauth.RemoteAppKey{{KID: signer.KID(), PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}}, group: coreauth.GroupInstance{ID: groupID, Persona: "merchant", InstanceSlug: "store"}}
	var mu sync.Mutex
	used := map[string]bool{}
	proofClaims := 0
	verifier := verify.NewVerifier(verify.WithDPoP(func(_ context.Context, key string, _ time.Duration) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		proofClaims++
		if used[key] {
			return false, nil
		}
		used[key] = true
		return true, nil
	}, func(r *http.Request) string { return origin + r.URL.EscapedPath() })).WithService(authority).
		WithPermissionChecker(authority, "https://authority.test")
	require.NoError(t, verifier.LoadRemoteApplications(t.Context(), authority, []string{"billing"}))
	integration, err := billingauth.NewIntegration(billingauth.IntegrationOptions{Verifier: verifier, Authority: func(context.Context, billingauth.Requirement) (billingauth.Authority, error) {
		return billingauth.Authority{Scope: auth.Scope{Authority: "https://authority.test", ID: groupID}, Permission: permissions.MerchantCatalogRead}, nil
	}})
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	public, err := key.PublicKey.Bytes()
	require.NoError(t, err)
	x, y := base64.RawURLEncoding.EncodeToString(public[1:33]), base64.RawURLEncoding.EncodeToString(public[33:])
	thumb := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`))
	access, err := signer.SignWithHeaders(t.Context(), map[string]any{"iss": issuer, "aud": "billing", "delegated_sub": "delegated-actor", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "permissions": []string{permissions.MerchantCatalogRead}, "cnf": map[string]any{"jkt": base64.RawURLEncoding.EncodeToString(thumb[:])}}, map[string]any{"typ": verify.DelegatedAccessTokenType})
	require.NoError(t, err)
	proof := func(method, path string) string {
		hash := sha256.Sum256([]byte(access))
		token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"htm": method, "htu": origin + path, "iat": time.Now().Unix(), "jti": uuid.NewString(), "ath": base64.RawURLEncoding.EncodeToString(hash[:])})
		token.Header["typ"] = "dpop+jwt"
		token.Header["jwk"] = map[string]any{"kty": "EC", "crv": "P-256", "x": x, "y": y}
		signed, err := token.SignedString(key)
		require.NoError(t, err)
		return signed
	}
	target := billingauth.Target{MerchantID: merchant.ID(uuid.New()), MerchantSlug: "store", AuthorityGroupID: groupID}
	directory := proofDirectory{row: merchants.Merchant{ID: target.MerchantID, Slug: "store", Status: merchants.StatusActive, PermissionGroupID: groupID}}
	gate := integrationGate{auth: integration, runtime: &app.Runtime{}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := integration.Authentication.AuthenticateRequest(r.Context(), r)
		if err != nil {
			w.WriteHeader(401)
			return
		}
		require.Equal(t, billingauth.DelegatedUser, identity.Kind)
		require.Contains(t, r.RequestURI, "/v2/merchant/")
		principal, err := gate.Authorize(r.Context(), r, permissions.MerchantCatalogRead)
		if err != nil {
			w.WriteHeader(403)
			return
		}
		require.Equal(t, target.MerchantID, principal.MerchantID)
		w.WriteHeader(200)
	})
	table := &router.Table{Entries: []router.Entry{{Method: "GET", Path: "/v1/merchant/proof", Handler: handler}, {Method: "POST", Path: "/v1/merchant/proof", Handler: handler}, {Method: "GET", Path: "/v1/merchant/other", Handler: handler}}}
	router.AddMerchantSelectorRoutes(table, "", func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
		return merchanttarget.Resolve(ctx, r, directory, merchant.ID{}, "")
	})
	mounted := table.Handler()
	call := func(method, path, signedProof string) int {
		r := requestauth.Begin(httptest.NewRequest(method, origin+path, nil))
		r.Header.Set("Authorization", "DPoP "+access)
		r.Header.Set("DPoP", signedProof)
		r.Header.Set(merchant.SlugHeader, "store")
		w := httptest.NewRecorder()
		mounted.ServeHTTP(w, r)
		return w.Code
	}
	const route = "/v2/merchant/proof"
	first := proof("GET", route)
	require.Equal(t, 200, call("GET", route, first))
	require.Equal(t, 1, proofClaims, "authentication plus operation authorization consumes one genuine proof")
	require.Equal(t, 401, call("GET", route, first), "identical proof cannot replay across requests")
	require.Equal(t, 401, call("POST", route, proof("GET", route)), "method mismatch")
	require.Equal(t, 401, call("GET", "/v2/merchant/other", proof("GET", route)), "URI mismatch")
	require.Equal(t, 401, call("GET", route, proof("GET", "/v1/merchant/proof")), "v2 URI is never rewritten before proof verification")
	require.Equal(t, 200, call("GET", route, proof("GET", route)), "a fresh correctly bound proof remains usable")
}
