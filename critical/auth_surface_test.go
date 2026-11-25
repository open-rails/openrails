//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// The standalone server's AuthKit is derived from its own settings: who may
// act where, and what a deployment can still set.
func TestStandaloneAuthSurface(t *testing.T) {
	t.Run("OR-J01-S27/a-trusted-issuers-staff-token-is-no-customer", func(t *testing.T) {
		w := newStandaloneWorld(t)
		id := uuid.NewString()
		var staff, customer billing.CustomerToken
		w.command(t, map[string]any{"Op": "external-user-token", "Merchant": "alpha", "ID": id, "Scopes": []string{billing.ScopeMerchant}}, &staff)
		status, raw := w.request(t, http.MethodGet, "/v1/me", staff.AccessToken, nil)
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), string(billing.CodePermissionRequired))
		_, err := w.alpha.GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(id)))
		require.Error(t, err, "the refused token admitted no customer")
		w.command(t, map[string]any{"Op": "external-user-token", "Merchant": "alpha", "ID": id, "Scopes": []string{"openid", billing.ScopeSelf}}, &customer)
		status, raw = w.request(t, http.MethodGet, "/v1/me", customer.AccessToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
	})

	t.Run("OR-J01-S28/own-accounts-need-a-second-factor", func(t *testing.T) {
		w := newStandaloneWorld(t, func(c map[string]any) { c["StaffMFA"] = true })
		var user struct{ ID, Email, Password string }
		w.command(t, map[string]any{"Op": "new-user"}, &user)
		// The browser's key: a sign-in proves it, and the session's tokens
		// go with its proofs.
		key := authtest.NewDPoPKey(t)
		call := func(path, token string, body any, prove bool) (int, []byte) {
			data, err := json.Marshal(body)
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.URL+path, bytes.NewReader(data))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			switch {
			case token != "":
				key.Authorize(t, req, token, "")
			case prove:
				req.Header.Set("DPoP", key.Proof(t, http.MethodPost, w.URL+path, "", ""))
			}
			response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
			require.NoError(t, err)
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			return response.StatusCode, raw
		}
		credentials := map[string]string{"identifier": user.Email, "password": user.Password}
		status, raw := call("/auth/v1/password/login", "", credentials, false)
		require.Equal(t, http.StatusUnauthorized, status, "a sign-in proves a DPoP key: %s", raw)
		require.Contains(t, string(raw), "sender_proof_required")
		login := func() map[string]any {
			status, raw := call("/auth/v1/password/login", "", credentials, true)
			require.Equal(t, http.StatusOK, status, string(raw))
			var out map[string]any
			require.NoError(t, json.Unmarshal(raw, &out))
			return out
		}
		first := login()
		require.Equal(t, "enrollment_required", first["status"], "a password alone is no session: %v", first)
		require.Nil(t, first["token_set"])
		step := first["enrollment"].(map[string]any)
		require.Contains(t, step["allowed_methods"], "totp")
		enrollment := step["token_set"].(map[string]any)["access_token"].(string)
		status, raw = w.request(t, http.MethodGet, "/auth/v1/capabilities", "", nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), `"totp"`, "authenticator apps are offered")
		status, raw = w.request(t, http.MethodPost, "/auth/v1/me/2fa/setup", enrollment, map[string]string{"method": "totp"})
		require.Equal(t, http.StatusUnauthorized, status, "a bound token is no bearer: %s", raw)
		status, raw = call("/auth/v1/me/2fa/setup", enrollment, map[string]string{"method": "totp"}, false)
		require.Equal(t, http.StatusOK, status, string(raw))
		var setup struct{ Secret string }
		require.NoError(t, json.Unmarshal(raw, &setup))
		status, raw = call("/auth/v1/me/2fa/factors", enrollment, map[string]string{"method": "totp", "code": authtest.TOTPCode(t, setup.Secret, time.Now())}, false)
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.Contains(t, string(raw), `"access_token"`, "enrolling finishes the sign-in")
		second := login()
		require.Equal(t, "second_factor_required", second["status"], "every sign-in asks for it: %v", second)
		require.Nil(t, second["token_set"])
	})

	t.Run("OR-J01-S31/a-merchants-own-site-runs-the-admin-screens-with-its-staff-token", func(t *testing.T) {
		w := newStandaloneWorld(t)
		// Each merchant has a customer.
		for _, slug := range []string{"alpha", "beta"} {
			status, raw := w.request(t, http.MethodGet, "/v1/me", w.Merchants[slug].CustomerToken, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
		}
		alphaCustomer, betaCustomer := w.Merchants["alpha"].CustomerID.String(), w.Merchants["beta"].CustomerID.String()
		// The merchant's app, its declared issuer, mints its staff member a
		// token for the server: openrails:merchant and their permissions.
		staff := func(permissions ...string) string {
			var token billing.CustomerToken
			w.command(t, map[string]any{"Op": "external-user-token", "Merchant": "alpha", "ID": uuid.NewString(), "Scopes": []string{billing.ScopeMerchant}, "Permissions": append([]string{}, permissions...)}, &token)
			return token.AccessToken
		}
		const site = "https://shop.alpha.critical.invalid"
		// The admin screens on the merchant's page call each staff group
		// cross-origin: any origin, never with credentials.
		for _, path := range []string{"/v1/admin/access", "/v1/admin/customers", "/v1/catalog/products", "/v1/merchant/psps", "/v1/metrics/schema"} {
			status, headers, _ := w.send(t, http.MethodOptions, path, "", map[string]string{"Origin": site, "Access-Control-Request-Method": http.MethodGet, "Access-Control-Request-Headers": "authorization"})
			require.Equal(t, http.StatusNoContent, status, path)
			require.Equal(t, "*", headers.Get("Access-Control-Allow-Origin"), path)
			require.Empty(t, headers.Get("Access-Control-Allow-Credentials"), path)
			require.Contains(t, strings.ToLower(headers.Get("Access-Control-Allow-Headers")), "authorization", path)
		}
		reader := staff(string(billing.MerchantCustomerRead))
		origin := map[string]string{"Origin": site}
		status, headers, raw := w.send(t, http.MethodGet, "/v1/admin/access", reader, origin)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Equal(t, "*", headers.Get("Access-Control-Allow-Origin"))
		var access billing.AdminAccess
		require.NoError(t, json.Unmarshal(raw, &access))
		require.Equal(t, billing.AccessRead, access.Admin)
		require.Equal(t, billing.AccessNone, access.Catalog)
		require.Equal(t, billing.AccessNone, access.MerchantConfig)
		require.Equal(t, w.Merchants["alpha"].ID.String(), access.ScopeID)
		status, _, raw = w.send(t, http.MethodGet, "/v1/admin/customers", reader, origin)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), alphaCustomer)
		require.NotContains(t, string(raw), betaCustomer)
		status, _, raw = w.send(t, http.MethodGet, "/v1/admin/customers/"+alphaCustomer, reader, origin)
		require.Equal(t, http.StatusOK, status, string(raw))
		// A staff member without the permission is refused.
		for _, token := range []string{staff(), staff(string(billing.MerchantCatalogRead))} {
			status, _, raw = w.send(t, http.MethodGet, "/v1/admin/customers", token, origin)
			require.Equal(t, http.StatusForbidden, status, string(raw))
			require.Contains(t, string(raw), string(billing.CodePermissionRequired))
		}
		status, _, raw = w.send(t, http.MethodGet, "/v1/catalog/products", reader, origin)
		require.Equal(t, http.StatusForbidden, status, string(raw))
		// Another merchant's data stays out of reach: its customer is not
		// found, a root: permission the app writes into its token reaches no
		// merchant, and naming the other merchant refuses the request.
		status, _, raw = w.send(t, http.MethodGet, "/v1/admin/customers/"+betaCustomer, reader, origin)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		status, _, raw = w.send(t, http.MethodGet, "/v1/admin/customers", staff(string(billing.RootCustomerRead)), origin)
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), string(billing.CodePermissionRequired))
		status, _, raw = w.send(t, http.MethodGet, "/v1/admin/customers", reader, map[string]string{"Origin": site, "OpenRails-Expected-Merchant": w.Merchants["beta"].ID.String()})
		require.Equal(t, http.StatusPreconditionFailed, status, string(raw))
		require.Contains(t, string(raw), string(billing.CodeMerchantMismatch))
	})

	t.Run("OR-J01-S32/the-console-offers-only-trusted-issuers-whose-sign-in-works", func(t *testing.T) {
		w := newStandaloneWorld(t, func(c map[string]any) { c["Console"] = true })
		type button struct {
			Key, URL, Name, Resource, Scope string
			ClientID                        string `json:"client_id"`
			RedirectURI                     string `json:"redirect_uri"`
		}
		read := func() (map[string]json.RawMessage, []button) {
			status, raw := w.request(t, http.MethodGet, "/admin/config.json", "", nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			var cfg map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &cfg))
			var buttons []button
			require.NoError(t, json.Unmarshal(cfg["issuers"], &buttons))
			return cfg, buttons
		}
		// Nothing configured: the server's own accounts alone, and no
		// merchant's app to look up.
		cfg, buttons := read()
		require.NotNil(t, buttons)
		require.Empty(t, buttons)
		require.NotContains(t, cfg, "app_sign_in")
		require.Contains(t, string(cfg["exchange"]), `"openrails-console"`)
		// Root's issuers: two that work, and four that would not.
		working, second := oidcIssuer(t, nil), oidcIssuer(t, nil)
		mismatched := oidcIssuer(t, func(m map[string]any) { m["issuer"] = "https://elsewhere.critical.invalid" })
		plain := oidcIssuer(t, func(m map[string]any) { m["code_challenge_methods_supported"] = []string{"plain"} })
		keyless := oidcIssuer(t, func(m map[string]any) { m["jwks_uri"] = m["issuer"].(string) + "/empty-jwks.json" })
		gone := httptest.NewServer(http.NotFoundHandler())
		unreachable := gone.URL
		gone.Close()
		for _, issuer := range []string{working, second, mismatched, plain, keyless, unreachable} {
			trustApplication(t, w, "root", issuer)
		}
		// A merchant's own app is never a console button.
		trustApplication(t, w, w.Merchants["beta"].ID.String(), oidcIssuer(t, nil))
		require.Eventually(t, func() bool {
			_, buttons = read()
			return len(buttons) == 2
		}, 30*time.Second, 200*time.Millisecond)
		got := []string{buttons[0].URL, buttons[1].URL}
		require.ElementsMatch(t, []string{working, second}, got)
		for _, b := range buttons {
			require.Equal(t, b.URL, b.Key)
			require.Equal(t, strings.TrimPrefix(b.URL, "http://"), b.Name)
			require.Equal(t, "openrails-console", b.ClientID)
			require.Equal(t, w.URL, b.Resource)
			require.Equal(t, "openid profile email openrails:merchant", b.Scope)
			require.Equal(t, w.URL+"/admin/callback", b.RedirectURI)
		}
		// Each hidden issuer is a refusal the operator sees, with why.
		var refusals map[string]string
		w.command(t, map[string]any{"Op": "refusals"}, &refusals)
		for issuer, why := range map[string]string{
			mismatched:  `names the issuer "https://elsewhere.critical.invalid"`,
			plain:       "PKCE S256",
			keyless:     "has no keys",
			unreachable: "OpenID Connect discovery",
		} {
			subject := "console.issuers." + issuer + ": "
			at := strings.Index(refusals["configuration"], subject)
			require.GreaterOrEqual(t, at, 0, "%s: %s", issuer, refusals["configuration"])
			refusal, _, _ := strings.Cut(refusals["configuration"][at:], "; ")
			require.Contains(t, refusal, why)
		}
		require.NotContains(t, refusals["configuration"], "console.issuers."+working+":")
		require.NotContains(t, refusals["configuration"], "console.issuers."+second+":")
	})

	t.Run("OR-J01-S30/run-time-applications-name-only-public-key-urls", func(t *testing.T) {
		w := newStandaloneWorld(t)
		alpha := w.Merchants["alpha"].ID
		var granted map[string]bool
		w.command(t, map[string]any{"Op": "grant-customer-owner", "Merchant": "alpha", "Account": "alice"}, &granted)
		path := "/auth/v1/groups/" + alpha.String() + "/remote-applications"
		for _, private := range []map[string]any{
			{"issuer": "https://app.critical.invalid", "jwks_uri": "http://127.0.0.1:9/jwks.json"},
			{"issuer": "https://app.critical.invalid", "jwks_uri": "https://10.0.0.5/jwks.json"},
			{"issuer": "https://app.critical.invalid", "jwks_uri": "https://keys.svc.cluster.local/jwks.json"},
			{"issuer": "http://127.0.0.1:9"},
		} {
			status, raw := w.request(t, http.MethodPost, path, w.AliceToken, private)
			require.Equal(t, http.StatusBadRequest, status, "%v: %s", private, raw)
			require.Contains(t, string(raw), "invalid_remote_application")
		}
		status, raw := w.request(t, http.MethodPost, path, w.AliceToken, map[string]any{"issuer": "https://app.critical.invalid", "jwks_uri": "https://app.critical.invalid/jwks.json"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		// The operator declares one on a private network.
		body, err := json.Marshal(iam.RemoteApplication{Issuer: "https://cluster.critical.invalid", Mode: iam.RemoteApplicationModeJWKS, JWKSURI: "http://127.0.0.1:9/jwks.json"})
		require.NoError(t, err)
		var stored iam.RemoteApplication
		w.command(t, map[string]any{"Op": "trust-application", "Merchant": alpha.String(), "Body": json.RawMessage(body)}, &stored)
		require.Equal(t, "http://127.0.0.1:9/jwks.json", stored.JWKSURI)
	})

	t.Run("OR-J06-S84/auth-settings-beyond-the-key-path-refuse-boot", func(t *testing.T) {
		w := newWorld(t)
		const derived = "OpenRails derives the server's AuthKit configuration; only auth.keys.path (AUTH_KEYS_PATH) is set"
		for _, c := range []struct {
			name  string
			extra map[string]any
			env   string
			says  []string
		}{
			{"key", map[string]any{"auth": map[string]any{"password": map[string]any{"min_length": 12}}}, "", []string{"auth.password.min_length (AUTH_PASSWORD_MIN_LENGTH)", derived}},
			{"env", nil, "AUTH_TOKEN_ISSUER=https://billing.critical.invalid", []string{"auth.token_issuer (AUTH_TOKEN_ISSUER)", derived}},
			{"private", nil, "AUTH_TOKEN_ALLOW_PRIVATE_NETWORK_JWKS=true", []string{"auth.token_allow_private_network_jwks (AUTH_TOKEN_ALLOW_PRIVATE_NETWORK_JWKS)", derived}},
			{"local", map[string]any{"local_sign_in": true}, "", []string{"local_sign_in / LOCAL_SIGN_IN was removed"}},
			{"console", map[string]any{"admin_console": map[string]any{"issuers": map[string]any{}}}, "", []string{"admin_console.issuers / ADMIN_CONSOLE_ISSUERS was removed", "openrails-console"}},
		} {
			cmd := hostCLICommand(t, w, c.extra, "book", "status")
			if c.env != "" {
				cmd.Env = append(cmd.Env, c.env)
			}
			out, err := cmd.CombinedOutput()
			require.Error(t, err, c.name)
			for _, says := range c.says {
				require.Contains(t, string(out), says, c.name)
			}
		}
		cmd := hostCLICommand(t, w, map[string]any{"auth": map[string]any{"keys": map[string]any{"path": t.TempDir()}}}, "book", "status")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "the key path alone loads: %s", out)
	})

	t.Run("OR-J06-S85/a-server-whose-staff-cannot-enroll-a-second-factor-is-not-ready", func(t *testing.T) {
		w := newWorld(t)
		t.Cleanup(func() {
			for _, schema := range []string{w.schema + "_auth", w.schema + "_river"} {
				_, _ = w.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			}
		})
		keys := t.TempDir()
		writeSigningKeys(t, keys)
		const missing = "authenticator apps are unavailable"
		// boot runs the binary on the world's database, a public issuer and
		// keys; ready asks /health/ready, and returns what it logged.
		boot := func() (ready func() (int, string), stop func()) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			port := listener.Addr().(*net.TCPAddr).Port
			require.NoError(t, listener.Close())
			cmd := hostCLICommand(t, w, map[string]any{"port": port, "public_url": "https://billing.critical.invalid", "auth": map[string]any{"keys": map[string]any{"path": keys}}}, "run-server")
			log := &lockedBuffer{}
			cmd.Stdout, cmd.Stderr = log, log
			require.NoError(t, cmd.Start())
			stop = func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
			t.Cleanup(stop)
			return func() (int, string) {
				response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health/ready", port))
				if err != nil {
					return 0, log.String()
				}
				defer response.Body.Close()
				_, _ = io.Copy(io.Discard, response.Body)
				return response.StatusCode, log.String()
			}, stop
		}
		ready, stop := boot()
		require.Eventually(t, func() bool {
			status, log := ready()
			return status == http.StatusServiceUnavailable && strings.Contains(log, missing)
		}, 90*time.Second, 500*time.Millisecond, "without totp.key staff can enroll no authenticator app")
		stop()
		key := make([]byte, 32)
		_, err := rand.Read(key)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(keys, "totp.key"), []byte(base64.StdEncoding.EncodeToString(key)), 0o600))
		ready, _ = boot()
		require.Eventually(t, func() bool {
			status, _ := ready()
			return status == http.StatusOK
		}, 90*time.Second, 500*time.Millisecond, "with its key mounted the server is ready")
	})
}

// lockedBuffer is a child process's output, read while it writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// writeSigningKeys writes an AuthKit keys.json under dir: one Ed25519
// signing key.
func writeSigningKeys(t *testing.T, dir string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(private)
	require.NoError(t, err)
	pub, err := x509.MarshalPKIXPublicKey(public)
	require.NoError(t, err)
	data, err := json.Marshal(map[string]any{
		"active_key_id":          "critical",
		"active_private_key_pem": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"public_keys":            map[string]string{"critical": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}))},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keys.json"), data, 0o600))
}

// oidcIssuer serves an OpenID Connect issuer's metadata and keys, as change
// leaves them; its URL is the issuer.
func oidcIssuer(t *testing.T, change func(map[string]any)) string {
	t.Helper()
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			m := map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/oauth2/authorize", "token_endpoint": issuer + "/oauth2/token", "jwks_uri": issuer + "/jwks.json",
				"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"}}
			if change != nil {
				change(m)
			}
			_ = json.NewEncoder(rw).Encode(m)
		case "/jwks.json":
			_ = json.NewEncoder(rw).Encode(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": "app", "x": "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"}}})
		case "/empty-jwks.json":
			_ = json.NewEncoder(rw).Encode(map[string]any{"keys": []any{}})
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(server.Close)
	issuer = server.URL
	return issuer
}

// trustApplication declares issuer the trusted application of group, a
// merchant's id or "root", with a static key.
func trustApplication(t *testing.T, w *standaloneWorld, group, issuer string) {
	t.Helper()
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(signer.Public())
	require.NoError(t, err)
	body, err := json.Marshal(iam.RemoteApplication{Issuer: issuer, Mode: iam.RemoteApplicationModeStatic, PublicKeys: []iam.RemoteApplicationKey{{KID: "app", PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}})
	require.NoError(t, err)
	var stored iam.RemoteApplication
	w.command(t, map[string]any{"Op": "trust-application", "Merchant": group, "Body": json.RawMessage(body)}, &stored)
}
