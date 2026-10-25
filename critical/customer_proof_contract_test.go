//go:build integration

package critical_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"
	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// Forward the original provider's authority metadata, and count only actual
// authentication. The resolver must consume the retained Verified, not replay it.
type proofAuthenticator struct{ bindingAuthenticator }

func (a *proofAuthenticator) RootScope(ctx context.Context) (auth.Scope, error) {
	return a.Authenticator.(auth.Root).RootScope(ctx)
}

func authenticateCustomerProof(t *testing.T, provider auth.Authenticator, token string, sender *authtest.DPoPKey) (auth.Verified, *http.Request) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "https://critical.invalid/customer-proof", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	if sender != nil {
		sender.Authorize(t, request, token, "")
	}
	verified, err := provider.Authenticate(request)
	var challenge *auth.Challenge
	if sender != nil && errors.As(err, &challenge) && challenge.Header.Get("DPoP-Nonce") != "" {
		sender.Authorize(t, request, token, challenge.Header.Get("DPoP-Nonce"))
		verified, err = provider.Authenticate(request)
	}
	require.NoError(t, err)
	return verified, request
}

func signCustomerProof(t *testing.T, signer keys.Signer, typ string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"typ": typ, "alg": signer.Algorithm(), "kid": signer.KID()})
	require.NoError(t, err)
	body, err := json.Marshal(claims)
	require.NoError(t, err)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	signature, err := signer.Sign(t.Context(), []byte(input))
	require.NoError(t, err)
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func requireProofCode(t *testing.T, err error, code string) {
	t.Helper()
	var status *billing.StatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, code, status.Code)
}

func TestCustomerProofResolution(t *testing.T) {
	t.Run("OR-J01-S12/existing-native-customer-proof-is-read-only", func(t *testing.T) {
		a := newAuthorityWorld(t)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, err := a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		require.NoError(t, err)
		before, err := a.alpha.GetCustomer(t.Context(), customer)
		require.NoError(t, err)
		provider := &proofAuthenticator{bindingAuthenticator{Authenticator: a.auth.Authenticator()}}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://critical.invalid/customer-proof", nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+a.aliceToken)
		verified, err := provider.Authenticate(request)
		require.NoError(t, err)
		for range 2 {
			binding, err := a.engine.ResolveCustomer(t.Context(), provider, verified)
			require.NoError(t, err)
			require.Equal(t, a.alpha.MerchantID(), binding.MerchantID)
			require.Equal(t, customer, binding.CustomerID)
			require.Nil(t, binding.Issuer, "the native customer's stored SQL NULL is preserved")
		}
		require.EqualValues(t, 1, provider.calls.Load())
		after, err := a.alpha.GetCustomer(t.Context(), customer)
		require.NoError(t, err)
		require.Equal(t, before.LastSeenAt, after.LastSeenAt, "reading proof must not refresh or admit the customer")
		other := billing.CustomerID(uuid.MustParse(a.bob.ID))
		request.Header.Set("Authorization", "Bearer "+a.bobToken)
		missing, err := provider.Authenticate(request)
		require.NoError(t, err)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, missing)
		require.ErrorIs(t, err, billing.ErrNotFound)
		_, err = a.alpha.GetCustomer(t.Context(), other)
		require.ErrorIs(t, err, billing.ErrNotFound, "proof resolution must not admit an absent customer")
		events, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"customer.network_linked"}, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, events.Events, "proof resolution must not announce platform enrollment")
		stale := authtest.StaleSession(t, a.auth, a.aliceToken)
		older, _ := authenticateCustomerProof(t, provider, stale, nil)
		require.ErrorIs(t, older.(auth.RecentSignInChecker).CheckRecentSignIn(t.Context()), auth.ErrStepUpRequired)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, older)
		require.NoError(t, err, "customer proof does not manufacture or require recent sign-in")
	})
	t.Run("OR-J01-S13/customer-proof-preserves-issuer-tenant-and-credential-boundaries", func(t *testing.T) {
		a := newAuthorityWorldWithAuth(t, func(c *authkit.Config) { c.PersonalAccessTokens.Enabled = true })
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, err := a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		require.NoError(t, err)
		provider := a.auth.Authenticator()
		verified, _ := authenticateCustomerProof(t, provider, a.aliceToken, nil)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, nil)
		require.ErrorIs(t, err, auth.ErrUnauthenticated)
		_, err = a.engine.ResolveCustomer(t.Context(), nil, verified)
		require.ErrorIs(t, err, auth.ErrUnavailable)
		// An identity-only wrapper has deliberately dropped the documented
		// capability. Matching metadata alone must never grant proof.
		_, err = a.engine.ResolveCustomer(t.Context(), provider, struct{ auth.Verified }{verified})
		require.ErrorIs(t, err, auth.ErrForbidden)
		key, _ := authenticateCustomerProof(t, provider, a.alphaKey.Secret, nil)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, key)
		require.ErrorIs(t, err, auth.ErrForbidden)
		pat, err := a.auth.CreatePersonalAccessToken(t.Context(), verified.Identity(), iam.NewPersonalAccessToken{Name: "customer proof denial", GroupID: a.alpha.MerchantID().String(), Permissions: []iam.Perm{mustPermission(t, string(billing.MerchantCustomerRead))}})
		require.NoError(t, err)
		personal, _ := authenticateCustomerProof(t, provider, pat.Secret, nil)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, personal)
		require.ErrorIs(t, err, auth.ErrForbidden)
		bound, _ := authenticateCustomerProof(t, provider, a.signedToken(t, func(_ map[string]any, claims map[string]any) {
			claims["group_id"] = a.beta.MerchantID().String()
		}), nil)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, bound)
		require.ErrorIs(t, err, billing.ErrNotFound, "a bound credential must not fall back to the issuer's other merchant")
		_, err = a.beta.GetCustomer(t.Context(), customer)
		require.ErrorIs(t, err, billing.ErrNotFound)

		customers := pgx.Identifier{a.schema, "customers"}.Sanitize()
		merchants := pgx.Identifier{a.schema, "merchants"}.Sanitize()
		// Seed an existing foreign namespace; the public resolver must
		// refuse it rather than rewrite the stored customer's issuer.
		_, err = a.pool.Exec(t.Context(), "UPDATE "+customers+" SET issuer=$1 WHERE merchant_id=$2 AND id=$3", "https://different-issuer.example", a.alpha.MerchantID().UUID(), customer.UUID())
		require.NoError(t, err)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, verified)
		requireProofCode(t, err, billing.CodeCustomerIssuerMismatch)
		_, err = a.pool.Exec(t.Context(), "UPDATE "+customers+" SET issuer=NULL WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), customer.UUID())
		require.NoError(t, err)
		_, err = a.beta.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		require.NoError(t, err)
		_, err = a.pool.Exec(t.Context(), "UPDATE "+merchants+" SET permission_group_id=id::text,tenant_id=$1 WHERE id=$2", uuid.New(), a.beta.MerchantID().UUID())
		require.NoError(t, err)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, bound)
		requireProofCode(t, err, billing.CodeCustomerTenantMismatch)
		_, err = a.pool.Exec(t.Context(), "UPDATE "+merchants+" SET permission_group_id=id::text,tenant_id=$1 WHERE id=$2", a.tenant.String(), a.alpha.MerchantID().UUID())
		require.NoError(t, err)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, verified)
		require.NoError(t, err)
		_, err = a.pool.Exec(t.Context(), "UPDATE "+merchants+" SET deleted_at=now() WHERE id=$1", a.alpha.MerchantID().UUID())
		require.NoError(t, err)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, verified)
		requireProofCode(t, err, billing.CodeMerchantUnresolved)
	})
	t.Run("OR-J01-S14/asserted-and-sender-bound-resource-proofs-retain-source-authority", func(t *testing.T) {
		const resource = "https://critical.invalid"
		a := newAuthorityWorldWithAuth(t, func(c *authkit.Config) {
			c.Resource = authkit.ResourceConfig{ID: resource, PublicURL: resource, Scopes: map[string][]string{billing.ScopeSelf: {}}}
		})
		jwk := keys.PublicJWK(a.signer.Public(), a.signer.KID(), a.signer.Algorithm())
		registration := iam.RemoteApplication{Issuer: "https://merchant-" + uuid.NewString() + ".example", Enabled: true, Mode: iam.RemoteApplicationModeStatic, PublicKeys: []iam.RemoteApplicationKey{{KID: a.signer.KID(), JWK: &jwk}}}
		app, err := a.auth.UpsertRemoteApplication(t.Context(), iam.SystemIdentity(), a.alphaGroup, registration)
		require.NoError(t, err)
		customer := billing.CustomerID(uuid.New())
		_, err = a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		require.NoError(t, err)
		now := time.Now()
		assertion := signCustomerProof(t, a.signer, "JWT", map[string]any{"iss": app.Issuer, "sub": customer.String(), "aud": authtest.Issuer + iam.OAuthTokenPath, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "jti": uuid.NewString()})
		server := httptest.NewServer(a.auth.Handler())
		defer server.Close()
		response, err := server.Client().PostForm(server.URL+iam.OAuthTokenPath, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}, "scope": {billing.ScopeSelf}})
		require.NoError(t, err)
		raw, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
		var token struct {
			AccessToken string `json:"access_token"`
		}
		require.NoError(t, json.Unmarshal(raw, &token))
		provider := &proofAuthenticator{bindingAuthenticator{Authenticator: a.auth.Authenticator()}}
		asserted, _ := authenticateCustomerProof(t, provider, token.AccessToken, nil)
		require.True(t, asserted.(auth.Asserted).Asserted())
		binding, err := a.engine.ResolveCustomer(t.Context(), provider, asserted)
		require.NoError(t, err)
		require.Equal(t, openrails.CustomerProof{MerchantID: a.alpha.MerchantID(), CustomerID: customer}, binding)

		sender := authtest.NewDPoPKey(t)
		claims := map[string]any{"iss": app.Issuer, "sub": customer.String(), "aud": resource, "client_id": "merchant-ui", "scope": billing.ScopeSelf, "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(), "jti": uuid.NewString(), "cnf": map[string]any{"jkt": sender.Thumbprint()}}
		foreign, request := authenticateCustomerProof(t, provider, signCustomerProof(t, a.signer, "at+jwt", claims), sender)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, foreign)
		requireProofCode(t, err, billing.CodeCustomerIssuerMismatch)
		// A direct foreign token retains its issuer even when its subject
		// equals the merchant assertion's native customer UUID.
		_, err = a.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{a.schema, "customers"}.Sanitize()+" SET issuer=$1 WHERE merchant_id=$2 AND id=$3", app.Issuer, a.alpha.MerchantID().UUID(), customer.UUID())
		require.NoError(t, err)
		calls := provider.calls.Load()
		for range 2 {
			got, err := a.engine.ResolveCustomer(t.Context(), provider, foreign)
			require.NoError(t, err)
			require.Equal(t, openrails.CustomerProof{MerchantID: binding.MerchantID, CustomerID: binding.CustomerID, Issuer: &app.Issuer}, got)
		}
		require.Equal(t, calls, provider.calls.Load())
		_, err = provider.Authenticate(request)
		require.ErrorIs(t, err, auth.ErrUnauthenticated, "the replay control proves the original sender proof was already spent")
		claims["act"] = map[string]string{"sub": uuid.NewString()}
		delegated, _ := authenticateCustomerProof(t, provider, signCustomerProof(t, a.signer, "at+jwt", claims), sender)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, delegated)
		requireProofCode(t, err, billing.CodeInvokerScopedPrincipal)
		delete(claims, "act")
		claims["sub"] = claims["client_id"]
		application, _ := authenticateCustomerProof(t, provider, signCustomerProof(t, a.signer, "at+jwt", claims), sender)
		require.Equal(t, auth.SubjectApplication, application.Identity().SubjectKind)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, application)
		require.ErrorIs(t, err, auth.ErrForbidden)
		claims["sub"] = customer.String()
		claims["scope"] = "openid"
		console, _ := authenticateCustomerProof(t, provider, signCustomerProof(t, a.signer, "at+jwt", claims), sender)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, console)
		requireProofCode(t, err, billing.CodePermissionRequired)
		app.Enabled = false
		_, err = a.auth.UpsertRemoteApplication(t.Context(), iam.SystemIdentity(), a.alphaGroup, app)
		require.NoError(t, err)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, foreign)
		require.ErrorIs(t, err, auth.ErrRevoked)
		_, err = a.engine.ResolveCustomer(t.Context(), provider, asserted)
		require.ErrorIs(t, err, auth.ErrRevoked)
	})
	t.Run("OR-J01-S15/expiry-revocation-and-storage-failure-refuse-retained-customer-proof", func(t *testing.T) {
		// Billing can operate without contact enrichment. Cutting the real
		// credential store must not also cut this independent billing read.
		a := newAuthorityWorld(t, func(w *world) { w.deps.UserInfo = nil })
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, err := a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		require.NoError(t, err)
		zero, err := a.auth.NewVerifier([]string{authtest.Audience}, verify.WithSkew(0))
		require.NoError(t, err)
		provider := zero.Authenticator()
		for _, mode := range []string{"expiry", "revocation"} {
			t.Run(mode, func(t *testing.T) {
				deadline := time.Now().UTC().Truncate(time.Second).Add(4 * time.Second)
				token := a.aliceToken
				if mode == "expiry" {
					token = a.signedToken(t, func(_ map[string]any, claims map[string]any) {
						claims["exp"] = deadline.Unix()
						claims["iat"], claims["auth_time"] = time.Now().Add(-2*time.Minute).Unix(), time.Now().Add(-2*time.Minute).Unix()
						delete(claims, "nbf")
					})
				}
				verified, _ := authenticateCustomerProof(t, provider, token, nil)
				tx, err := a.pool.Begin(t.Context())
				require.NoError(t, err)
				defer tx.Rollback(context.Background())
				table := pgx.Identifier{a.schema, "customers"}.Sanitize()
				_, err = tx.Exec(t.Context(), "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE")
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() { _, err := a.engine.ResolveCustomer(ctx, provider, verified); done <- err }()
				require.Eventually(t, func() bool {
					var blocked bool
					err := a.pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation=$1::regclass AND NOT granted)", table).Scan(&blocked)
					require.NoError(t, err)
					return blocked
				}, 2*time.Second, 10*time.Millisecond, "resolver must reach the real customer read before the credential ends")
				want := auth.ErrRevoked
				if mode == "expiry" {
					time.Sleep(time.Until(deadline.Add(30 * time.Millisecond)))
					want = auth.ErrExpired
				} else {
					_, err = a.auth.RevokeAccountSessions(t.Context(), iam.UserIdentity(a.alice.ID), a.alice.ID)
					require.NoError(t, err)
				}
				require.NoError(t, tx.Rollback(t.Context()))
				require.ErrorIs(t, <-done, want)
			})
		}
		fresh := authtest.SignIn(t, a.auth, a.alice)
		verified, _ := authenticateCustomerProof(t, provider, fresh.AccessToken, nil)
		a.storage.cut()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err = a.engine.ResolveCustomer(ctx, provider, verified)
		require.ErrorIs(t, err, auth.ErrUnavailable)
		_, err = a.alpha.GetCustomer(t.Context(), customer)
		require.NoError(t, err, "the billing customer remains when proof authority is unavailable")
	})
}
