//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

type authorityWorld struct {
	*world
	storage                 *storageLink
	auth                    *authkit.Client
	authPool                *pgxpool.Pool
	tenant                  iam.TenantID
	server                  *httptest.Server
	alice, bob              authtest.User
	aliceToken, bobToken    string
	alphaKey, betaKey       iam.APIKeyCreated
	alphaGroup, betaGroup   iam.GroupRef
	alphaRemote, betaRemote *openrails.Client
	merchant                *authkit.PersonaDef
	reader                  iam.Role
	signer                  keys.Signer
}

func newAuthorityWorld(t *testing.T, configure ...func(*world)) *authorityWorld {
	t.Helper()
	a := &authorityWorld{tenant: iam.NewTenantID()}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	a.signer, err = keys.SignerFromKey("critical-issuer", private)
	require.NoError(t, err)
	roles := authkit.NewRoles(authkit.APIKeys)
	a.merchant = roles.Persona(billing.MerchantGroupPersona, authkit.APIKeys, authkit.CustomRoles)
	for _, permission := range billing.Permissions() {
		if permission.Persona() == billing.MerchantGroupPersona {
			a.merchant.Declare(string(permission))
		} else {
			roles.Root.Declare(string(permission))
		}
	}
	a.reader = a.merchant.Role("viewer", mustPermission(t, string(billing.MerchantCustomerRead)))
	a.world = newWorld(t, func(w *world) {
		var err error
		pc := w.pool.Config().Copy()
		a.storage = newStorageLink(t, net.JoinHostPort(pc.ConnConfig.Host, strconv.Itoa(int(pc.ConnConfig.Port))))
		pc.ConnConfig.Host = "127.0.0.1"
		pc.ConnConfig.Port = uint16(a.storage.listener.Addr().(*net.TCPAddr).Port)
		pc.ConnConfig.ConnectTimeout = time.Second
		a.authPool, err = pgxpool.NewWithConfig(t.Context(), pc)
		require.NoError(t, err)
		t.Cleanup(a.authPool.Close)
		authSchema := w.schema + "_auth"
		t.Cleanup(func() {
			_, err := w.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{authSchema}.Sanitize()+" CASCADE")
			require.NoError(t, err)
		})
		a.auth, _ = authtest.New(t, authtest.WithConfig(func(c *authkit.Config) {
			c.DefaultTenantID = a.tenant
			c.Database.Schema, c.Database.RiverSchema = authSchema, authSchema
			c.Roles = roles
			c.TwoFactor.Mode = iam.TwoFactorDisabled
		}), authtest.WithDeps(func(d *authkit.Deps) {
			d.Postgres = a.authPool
			d.KeySource = keys.Static{Active: a.signer, Public: map[string]crypto.PublicKey{a.signer.KID(): a.signer.Public()}}
		}))
		w.cfg.Merchants["alpha"] = openrails.MerchantDeclaration{DisplayName: "Alpha", Issuer: authtest.Issuer}
		w.deps.UserInfo = a.auth.UserInfo()
		for _, change := range configure {
			change(w)
		}
	})
	a.alice, a.bob = authtest.NewUser(t, a.auth), authtest.NewUser(t, a.auth)
	a.aliceToken = authtest.SignIn(t, a.auth, a.alice).AccessToken
	a.bobToken = authtest.SignIn(t, a.auth, a.bob).AccessToken
	owner := iam.UserSubject(a.alice.ID)
	for _, item := range []struct {
		client *openrails.Client
		group  *iam.GroupRef
		key    *iam.APIKeyCreated
	}{
		{a.alpha, &a.alphaGroup, &a.alphaKey}, {a.beta, &a.betaGroup, &a.betaKey},
	} {
		group, err := a.auth.CreateGroup(t.Context(), iam.NewGroup{ID: item.client.MerchantID().String(), TenantID: a.tenant, Persona: a.merchant.Persona, Owner: &owner})
		require.NoError(t, err)
		*item.group = iam.GroupByID(group.ID)
		*item.key, err = a.auth.CreateAPIKey(t.Context(), iam.SystemIdentity(), *item.group, iam.NewAPIKey{Name: "merchant-backend", Role: a.merchant.Owner})
		require.NoError(t, err)
	}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, a.engine, openrails.Routes{Auth: a.auth.Authenticator(), RouteGroups: openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true}}))
	a.server = httptest.NewServer(mux)
	a.server.Client().Timeout = 5 * time.Second
	t.Cleanup(a.server.Close)
	a.alphaRemote = a.remote(t, a.alpha.MerchantID(), a.alphaKey.Secret)
	a.betaRemote = a.remote(t, a.beta.MerchantID(), a.betaKey.Secret)
	return a
}

func mustPermission(t *testing.T, text string) iam.Perm {
	t.Helper()
	var p iam.Perm
	require.NoError(t, p.UnmarshalText([]byte(text)))
	return p
}

func (a *authorityWorld) remote(t *testing.T, merchant billing.MerchantID, credential string) *openrails.Client {
	t.Helper()
	r, err := openrails.NewRemote(a.server.URL, openrails.RemoteOptions{Credentials: map[billing.MerchantID]openrails.RemoteCredential{merchant: {APIKey: credential}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close(context.Background())) })
	c, err := r.Merchant(merchant)
	require.NoError(t, err)
	return c
}

func (a *authorityWorld) request(t *testing.T, method, path, credential string, body any, extraHeaders ...map[string]string) (int, []byte) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		require.NoError(t, err)
	}
	r, err := http.NewRequestWithContext(t.Context(), method, a.server.URL+path, bytes.NewReader(data))
	require.NoError(t, err)
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, headers := range extraHeaders {
		for key, value := range headers {
			r.Header.Set(key, value)
		}
	}
	res, err := a.server.Client().Do(r)
	require.NoError(t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, raw
}

func TestAuthority(t *testing.T) {
	t.Run("OR-J01-S06/human-cannot-use-application-authority", func(t *testing.T) {
		a := newAuthorityWorld(t)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "authority-control"}})
		require.NoError(t, err)
		before, err := a.alpha.GetBalance(t.Context(), customer, "USD")
		require.NoError(t, err)
		status, raw := a.request(t, "POST", "/v1/app/admissions", a.aliceToken, billing.AdmitBatchParams{Items: []billing.AdmitParams{{RequestID: "not-a-machine", CustomerID: customer, Currency: "USD", InvokerType: billing.InvokerTypeCustomer}}})
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), `"code":"application_required"`)
		after, err := a.alpha.GetBalance(t.Context(), customer, "USD")
		require.NoError(t, err)
		require.Equal(t, before.BalanceAmount, after.BalanceAmount)
		require.Equal(t, before.HeldAmount, after.HeldAmount)
		admissions, err := a.alpha.ListAdmissions(t.Context(), billing.AdmissionListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Empty(t, admissions.Items)
		status, raw = a.request(t, "GET", "/v1/me", a.alphaKey.Secret, nil)
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), `"code":"invoker_scoped_principal"`)
	})
}

// Sign deliberately invalid authority claims with the real configured issuer
// key. The actual AuthKit verifier remains in the HTTP request path.
func (a *authorityWorld) signedToken(t *testing.T, change func(map[string]any, map[string]any)) string {
	t.Helper()
	parts := strings.Split(a.aliceToken, ".")
	require.Len(t, parts, 3)
	var header, claims map[string]any
	for i, into := range []*map[string]any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, into))
	}
	change(header, claims)
	h, err := json.Marshal(header)
	require.NoError(t, err)
	c, err := json.Marshal(claims)
	require.NoError(t, err)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	signature, err := a.signer.Sign(t.Context(), []byte(input))
	require.NoError(t, err)
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}
