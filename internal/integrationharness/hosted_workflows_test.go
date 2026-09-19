//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
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
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/testauth"
	"github.com/open-rails/openrails/permissions"
)

// TestHostedMerchantsIsolateOneSubject: two merchants provisioned by two
// registered owners on ONE shared engine behind the hosted control plane.
// The same external subject funds and is entitled at both, through each
// merchant's owner-minted API key over HTTP and through the host's in-process
// Client, and every read resolves only that merchant's facts. Authority does
// not cross either: a key bound to the other merchant is refused, an owner
// cannot mint keys for the other merchant, and the other merchant's Client
// cannot see the receipts.
func TestHostedMerchantsIsolateOneSubject(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	hosted := h.StartHosted("USD")
	ownerA, ownerB := hosted.RegisterUser("owner-a"), hosted.RegisterUser("owner-b")
	a := hosted.ProvisionMerchant(ownerA, "isolate-a-"+uuid.NewString()[:8])
	b := hosted.ProvisionMerchant(ownerB, "isolate-b-"+uuid.NewString()[:8])
	require.NotEqual(t, a.ID, b.ID)

	subject := openrails.CustomerID(uuid.New())
	type side struct {
		merchant *HostedMerchant
		amount   int64
		feature  string
		sourceID string
		receipt  uuid.UUID
	}
	sides := []*side{
		{merchant: a, amount: 1_000_000, feature: "access-a", sourceID: uuid.NewString()},
		{merchant: b, amount: 2_000_000, feature: "access-b", sourceID: uuid.NewString()},
	}
	for _, s := range sides {
		remote, engine := s.merchant.Client(), s.merchant.EngineClient()
		request := openrails.DepositCreditsRequest{CustomerID: &subject, Invoker: subject.String(), Currency: "USD", Amount: s.amount, Source: "shared-subject", SourceID: s.sourceID}
		first, err := remote.DepositCredits(ctx, request)
		require.NoError(t, err)
		require.False(t, first.Replayed)
		require.Equal(t, s.amount, first.Amount)
		s.receipt = first.ID
		replay, err := engine.DepositCredits(ctx, request)
		require.NoError(t, err)
		require.True(t, replay.Replayed, "the host's in-process Client sees the HTTP deposit")
		require.Equal(t, first.ID, replay.ID)
		request.SourceID = uuid.NewString()
		_, err = engine.DepositCredits(ctx, request)
		require.NoError(t, err)
		_, err = remote.GrantEntitlement(ctx, subject, openrails.GrantEntitlementRequest{Entitlement: s.feature})
		require.NoError(t, err)
	}
	// Read only after both merchants have written.
	for i, s := range sides {
		other := sides[1-i]
		for name, client := range map[string]*openrails.Client{"http": s.merchant.Client(), "engine": s.merchant.EngineClient()} {
			balance, err := client.Balance(ctx, subject)
			require.NoError(t, err, name)
			require.Equal(t, 2*s.amount, balance.BalanceAmount, "%s %s", s.merchant.Slug, name)
			receipt, err := client.GetDeposit(ctx, subject, s.sourceID)
			require.NoError(t, err)
			require.Equal(t, s.receipt, receipt.ID)
			_, err = client.GetDeposit(ctx, subject, other.sourceID)
			require.ErrorIs(t, err, openrails.ErrNotFound, "the other merchant's receipt is invisible")
			own, err := client.HasEntitlement(ctx, subject, s.feature, time.Time{})
			require.NoError(t, err)
			require.True(t, own)
			foreign, err := client.HasEntitlement(ctx, subject, other.feature, time.Time{})
			require.NoError(t, err)
			require.False(t, foreign)
			records, err := client.ListActiveEntitlements(ctx, []openrails.CustomerID{subject}, time.Time{})
			require.NoError(t, err)
			require.Len(t, records[subject], 1)
			require.Equal(t, s.feature, records[subject][0].Entitlement)
		}
	}
	// Authority: a credential is bound to its merchant; owners hold no
	// authority over each other's merchants.
	crossBound, err := openrails.NewRemote(hosted.BaseURL, openrails.WithAPIKey(a.APIKey), openrails.WithMerchantID(b.ID))
	require.NoError(t, err)
	require.ErrorIs(t, crossBound.Verify(ctx), openrails.ErrConflict, "A's key cannot act as B")
	status, body := hosted.postJSON("/auth/merchant/"+b.Slug+"/api-keys", ownerA.Session, map[string]string{"name": "intruder", "role": "owner"})
	require.Equal(t, http.StatusForbidden, status, "%v", body)
	status, body = hosted.postJSON("/auth/merchant/"+a.Slug+"/api-keys", ownerB.Session, map[string]string{"name": "intruder", "role": "viewer"})
	require.Equal(t, http.StatusForbidden, status, "%v", body)
	viewer, err := openrails.NewRemote(hosted.BaseURL, openrails.WithAPIKey(a.MintAPIKey("viewer", "viewer")), openrails.WithMerchantID(a.ID))
	require.NoError(t, err)
	_, err = viewer.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &subject, Invoker: subject.String(), Currency: "USD", Amount: 1, Source: "shared-subject", SourceID: uuid.NewString()})
	require.ErrorIs(t, err, openrails.ErrDenied, "an owner-minted viewer key cannot move money")
}

// hostedIssuer is a merchant-owned AuthKit issuer with a browser user: the
// same engine the browser-auth job drives, minting DPoP-bound delegated
// tokens from a password session.
type hostedIssuer struct {
	server *httptest.Server
	signer *jwtkit.RSASigner
	access string
}

func newHostedIssuer(t *testing.T, h *Harness) *hostedIssuer {
	t.Helper()
	ctx := context.Background()
	issuer := httptest.NewUnstartedServer(nil)
	t.Cleanup(issuer.Close)
	issuerURL := "http://" + issuer.Listener.Addr().String()
	schema := "hosted_issuer_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	dbtest.ApplyAuthKitMigrations(t, ctx, h.sharedPool(), schema)
	signer, err := jwtkit.NewRSASigner(2048, "hosted-issuer")
	require.NoError(t, err)
	engine, err := authcore.New(authcore.Config{
		Schema:       schema,
		Keys:         authcore.KeysConfig{Source: jwtkit.StaticKeySource{Active: signer, Pubs: map[string]crypto.PublicKey{signer.KID(): signer.PublicKey()}}},
		Token:        authcore.TokenConfig{Issuer: issuerURL, IssuedAudiences: []string{"merchant"}, ExpectedAudiences: []string{"merchant"}},
		Registration: authcore.RegistrationConfig{Verification: authkit.RegistrationVerificationNone},
		Delegated:    authcore.DelegatedConfig{Audiences: []string{"openrails"}, AllowDPoP: true},
	}, authcore.Deps{Postgres: h.sharedPool(), Redis: h.Redis, DelegatedAuthorization: func(context.Context, authkit.DelegationRequest) (authkit.DelegationGrant, error) {
		return authkit.DelegationGrant{Permissions: []string{permissions.MerchantAll}}, nil
	}})
	require.NoError(t, err)
	svc, err := authhttp.New(engine, authhttp.Config{DirectPeerIP: true, DisableRateLimiting: true})
	require.NoError(t, err)
	t.Cleanup(svc.Close)
	handler, err := authhttp.MountHandler(svc, authhttp.MountOptions{APIPrefix: "/auth"})
	require.NoError(t, err)
	issuer.Config.Handler = handler
	issuer.Start()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	email, password := "member"+suffix+"@example.test", "Member-proof-2026!"
	user, err := engine.CreateUser(ctx, email, "member"+suffix)
	require.NoError(t, err)
	require.NoError(t, engine.AdminSetPassword(ctx, user.ID, password))
	response, err := issuer.Client().Post(issuer.URL+"/auth/password/login", "application/json", strings.NewReader(`{"identifier":"`+email+`","password":"`+password+`"}`))
	require.NoError(t, err)
	var session authkit.TokenSet
	require.NoError(t, json.NewDecoder(response.Body).Decode(&session))
	response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NotEmpty(t, session.AccessToken)
	return &hostedIssuer{server: issuer, signer: signer, access: session.AccessToken}
}

func (i *hostedIssuer) publicKeyPEM(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(i.signer.PublicKey())
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// mintDelegated asks the issuer for a delegated access token bound to sender.
func (i *hostedIssuer) mintDelegated(t *testing.T, sender *testauth.Sender) string {
	t.Helper()
	target := i.server.URL + "/auth/delegated/token"
	proof, err := sender.Proof(http.MethodPost, target, i.access)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(`{"audiences":["openrails"],"requested_grant":{}}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+i.access)
	req.Header.Set("DPoP", proof)
	response, err := i.server.Client().Do(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	var token struct {
		Token     string `json:"token"`
		TokenType string `json:"token_type"`
	}
	require.NoError(t, json.Unmarshal(raw, &token))
	require.Equal(t, "DPoP", token.TokenType)
	return token.Token
}

// senderBoundTransport presents the delegated token under the DPoP scheme with
// a fresh ES256 proof per request: the browser-side of a sender-constrained
// shared Client.
type senderBoundTransport struct {
	sender *testauth.Sender
	token  string
	// fixedProof, when set, replays one proof on every request.
	fixedProof string
}

func (s senderBoundTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	target := *req.URL
	target.RawQuery, target.Fragment = "", ""
	proof := s.fixedProof
	if proof == "" {
		var err error
		if proof, err = s.sender.Proof(req.Method, target.String(), s.token); err != nil {
			return nil, err
		}
	}
	req.Header.Set("Authorization", "DPoP "+s.token)
	req.Header.Set("DPoP", proof)
	return http.DefaultTransport.RoundTrip(req)
}

func hostedRequest(t *testing.T, method, url, session string, payload any) (int, string) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+session)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}

// TestHostedDelegationSenderBoundClient: on the hosted deployment a merchant
// owner registers their own AuthKit issuer over the hosted routes, that
// issuer mints an ES256 DPoP-bound delegated token from a user's password
// session, and the token drives the shared Client against the owner's
// merchant with real money and access effects. The token is refused for the
// other merchant, without its proof, with another sender's proof and with a
// replayed proof; disabling the issuer withdraws it.
func TestHostedDelegationSenderBoundClient(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	hosted := h.StartHosted("USD")
	ownerA, ownerB := hosted.RegisterUser("owner-a"), hosted.RegisterUser("owner-b")
	a := hosted.ProvisionMerchant(ownerA, "delegate-a-"+uuid.NewString()[:8])
	b := hosted.ProvisionMerchant(ownerB, "delegate-b-"+uuid.NewString()[:8])
	issuer := newHostedIssuer(t, h)
	// Registry refreshes are activity-driven within this TTL.
	cp := embcp.Get(app.HostGraph(hosted.Runtime()))
	cp.SetIssuerRegistryTTL(100 * time.Millisecond)
	defer cp.SetIssuerRegistryTTL(0)

	appSlug := "browser-" + uuid.NewString()[:8]
	registration := map[string]any{
		"slug": appSlug, "issuer": issuer.server.URL, "mode": "static", "enabled": true,
		"public_keys": []map[string]string{{"kid": issuer.signer.KID(), "public_key_pem": issuer.publicKeyPEM(t)}},
	}
	status, raw := hostedRequest(t, http.MethodPost, hosted.BaseURL+"/auth/merchant/"+a.Slug+"/remote-applications", ownerB.Session, registration)
	require.Equal(t, http.StatusForbidden, status, "another owner cannot register an issuer for A: %s", raw)
	status, raw = hostedRequest(t, http.MethodPost, hosted.BaseURL+"/auth/merchant/"+a.Slug+"/remote-applications", ownerA.Session, registration)
	require.Equal(t, http.StatusCreated, status, raw)
	status, raw = hostedRequest(t, http.MethodPut, hosted.BaseURL+"/auth/merchant/"+a.Slug+"/remote-applications/"+appSlug+"/roles/owner", ownerA.Session, nil)
	require.Equal(t, http.StatusOK, status, raw)

	sender, err := testauth.NewSender()
	require.NoError(t, err)
	token := issuer.mintDelegated(t, sender)
	bound := func(merchant openrails.MerchantID, transport http.RoundTripper) *openrails.Client {
		client, err := openrails.NewRemote(hosted.BaseURL,
			openrails.WithHTTPClient(&http.Client{Transport: transport}),
			openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil }),
			openrails.WithMerchantID(merchant), openrails.WithCurrency("USD"), openrails.WithTimeout(30*time.Second))
		require.NoError(t, err)
		return client
	}
	client := bound(a.ID, senderBoundTransport{sender: sender, token: token})
	require.Eventually(t, func() bool { return client.Verify(ctx) == nil }, 20*time.Second, 200*time.Millisecond,
		"the issuer registered over the hosted routes becomes usable within the registry refresh window")

	// The customer self route and the merchant routes share one verifier.
	statusReq := testauth.Request(ctx, http.MethodGet, hosted.BaseURL+"/v1/me/status", "")
	proof, err := sender.Proof(http.MethodGet, hosted.BaseURL+"/v1/me/status", token)
	require.NoError(t, err)
	statusReq.Header.Set("Authorization", "DPoP "+token)
	statusReq.Header.Set("DPoP", proof)
	statusResp, err := http.DefaultClient.Do(statusReq)
	require.NoError(t, err)
	statusResp.Body.Close()
	require.Equal(t, http.StatusOK, statusResp.StatusCode)

	subject := openrails.CustomerID(uuid.New())
	deposit, err := client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &subject, Invoker: subject.String(), Currency: "USD", Amount: 250_000, Source: "delegated", SourceID: uuid.NewString()})
	require.NoError(t, err)
	require.EqualValues(t, 250_000, deposit.Amount)
	balance, err := client.Balance(ctx, subject)
	require.NoError(t, err)
	require.EqualValues(t, 250_000, balance.BalanceAmount)
	_, err = client.GrantEntitlement(ctx, subject, openrails.GrantEntitlementRequest{Entitlement: "delegated-access"})
	require.NoError(t, err)
	allowed, err := client.HasEntitlement(ctx, subject, "delegated-access", time.Time{})
	require.NoError(t, err)
	require.True(t, allowed)
	invoices, _, err := client.ListMerchantInvoices(ctx, openrails.MerchantInvoiceFilter{}, 5, 0)
	require.NoError(t, err)
	require.NotNil(t, invoices)

	// The other merchant sees none of it, through its own owner key.
	otherBalance, err := b.Client().Balance(ctx, subject)
	require.NoError(t, err)
	require.Zero(t, otherBalance.BalanceAmount)
	foreign, err := b.Client().HasEntitlement(ctx, subject, "delegated-access", time.Time{})
	require.NoError(t, err)
	require.False(t, foreign)

	// Refusals: wrong merchant binding, missing proof, another sender's
	// proof, replayed proof.
	require.ErrorIs(t, bound(b.ID, senderBoundTransport{sender: sender, token: token}).Verify(ctx), openrails.ErrConflict)
	require.ErrorIs(t, bound(a.ID, http.DefaultTransport).Verify(ctx), openrails.ErrUnauthorized, "bearer presentation of a sender-bound token")
	other, err := testauth.NewSender()
	require.NoError(t, err)
	require.ErrorIs(t, bound(a.ID, senderBoundTransport{sender: other, token: token}).Verify(ctx), openrails.ErrUnauthorized)
	replayProof, err := sender.Proof(http.MethodGet, hosted.BaseURL+"/v1/merchant/settings", token)
	require.NoError(t, err)
	replayer := bound(a.ID, senderBoundTransport{sender: sender, token: token, fixedProof: replayProof})
	_, err = replayer.GetMerchantSettings(ctx)
	require.NoError(t, err)
	_, err = replayer.GetMerchantSettings(ctx)
	require.ErrorIs(t, err, openrails.ErrUnauthorized, "a proof is single use")

	// The owner withdraws the issuer through the hosted route.
	status, raw = hostedRequest(t, http.MethodDelete, hosted.BaseURL+"/auth/merchant/"+a.Slug+"/remote-applications/"+appSlug, ownerA.Session, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, status, raw)
	require.Eventually(t, func() bool { return client.Verify(ctx) != nil }, 20*time.Second, 200*time.Millisecond, "a withdrawn issuer's tokens stop verifying")
	require.ErrorIs(t, client.Verify(ctx), openrails.ErrUnauthorized)
}
