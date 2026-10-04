package embedhttp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"
	auth "github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/stretchr/testify/require"
)

// proofAuthority is a real DB-less verifier with a static permission check:
// the test is about the sender proof, not AuthKit's authority.
type proofAuthority struct {
	*verify.Verifier
	group string
}

func (a proofAuthority) Can(_ context.Context, _ iam.Actor, ref iam.GroupRef, perm iam.Perm) (bool, error) {
	return ref.ID() == a.group && perm.String() == billing.MerchantCatalogRead, nil
}
func (proofAuthority) KnownPermission(iam.Perm) bool { return true }

type proofVerifier struct{ proofAuthority }

func (v proofVerifier) AuthenticateRequest(ctx context.Context, r *http.Request) (auth.Principal, error) {
	return verify.AuthenticateRequest(ctx, v.proofAuthority, r)
}

type proofDirectory struct{ row merchants.Merchant }

func (d proofDirectory) Get(context.Context, billing.MerchantID) (*merchants.Merchant, error) {
	row := d.row
	return &row, nil
}
func (d proofDirectory) GetBySlug(context.Context, string) (*merchants.Merchant, error) {
	row := d.row
	return &row, nil
}

func TestDPoPProofVerifiedOnceAcrossV2RouteAndAuthorization(t *testing.T) {
	const issuer = "https://delegating-app.test"
	const origin = "https://billing.test"
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	require.NoError(t, err)
	groupID := uuid.NewString()
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
	}), verify.WithPublicURL(origin))
	require.NoError(t, verifier.AddIssuer(issuer, []string{"billing"}, verify.IssuerOptions{Keys: []iam.RemoteApplicationKey{{KID: "proof-issuer", PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}}))
	integration, err := billingauth.NewIntegration(billingauth.IntegrationOptions{Verifier: proofVerifier{proofAuthority{verifier, groupID}}, Authority: func(context.Context, billingauth.Requirement) (billingauth.Authority, error) {
		return billingauth.Authority{Scope: auth.Scope{Authority: issuer, ID: groupID}, Permission: billing.MerchantCatalogRead}, nil
	}})
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	public, err := key.PublicKey.Bytes()
	require.NoError(t, err)
	x, y := base64.RawURLEncoding.EncodeToString(public[1:33]), base64.RawURLEncoding.EncodeToString(public[33:])
	thumb := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`))
	delegated := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": issuer, "aud": "billing", "delegated_sub": "delegated-actor", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "permissions": []string{billing.MerchantCatalogRead}, "cnf": map[string]any{"jkt": base64.RawURLEncoding.EncodeToString(thumb[:])}})
	delegated.Header["typ"], delegated.Header["kid"] = "delegated-access+jwt", "proof-issuer"
	access, err := delegated.SignedString(rsaKey)
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
	target := billingauth.Target{MerchantID: billing.MerchantID(uuid.New()), MerchantSlug: "store", AuthorityGroupID: groupID}
	directory := proofDirectory{row: merchants.Merchant{ID: target.MerchantID, Slug: "store", Status: merchants.StatusActive, PermissionGroupID: groupID}}
	gate := integrationGate{auth: integration, runtime: &app.Runtime{}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := integration.Authentication.AuthenticateRequest(r.Context(), r)
		if err != nil {
			w.WriteHeader(401)
			return
		}
		require.Equal(t, billingauth.Delegated, identity.Kind)
		require.Contains(t, r.RequestURI, "/v2/merchant/")
		principal, err := gate.Authorize(r.Context(), r, billing.MerchantCatalogRead)
		if err != nil {
			w.WriteHeader(403)
			return
		}
		require.Equal(t, target.MerchantID, principal.MerchantID)
		w.WriteHeader(200)
	})
	table := &router.Table{Entries: []router.Entry{{Method: "GET", Path: "/v1/merchant/proof", Handler: handler}, {Method: "POST", Path: "/v1/merchant/proof", Handler: handler}, {Method: "GET", Path: "/v1/merchant/other", Handler: handler}}}
	router.AddMerchantSelectorRoutes(table, "", func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
		return merchanttarget.Resolve(ctx, r, directory, billing.MerchantID{}, "")
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
