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
		login := func() map[string]any {
			status, raw := w.request(t, http.MethodPost, "/auth/v1/password/login", "", map[string]string{"identifier": user.Email, "password": user.Password})
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
		status, raw := w.request(t, http.MethodGet, "/auth/v1/capabilities", "", nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), `"totp"`, "authenticator apps are offered")
		status, raw = w.request(t, http.MethodPost, "/auth/v1/me/2fa/setup", enrollment, map[string]string{"method": "totp"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var setup struct{ Secret string }
		require.NoError(t, json.Unmarshal(raw, &setup))
		status, raw = w.request(t, http.MethodPost, "/auth/v1/me/2fa/factors", enrollment, map[string]string{"method": "totp", "code": authtest.TOTPCode(t, setup.Secret, time.Now())})
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.Contains(t, string(raw), `"access_token"`, "enrolling finishes the sign-in")
		second := login()
		require.Equal(t, "second_factor_required", second["status"], "every sign-in asks for it: %v", second)
		require.Nil(t, second["token_set"])
	})

	t.Run("OR-J01-S29/console-sign-in-is-the-trusted-issuers-that-sign-people-in", func(t *testing.T) {
		w := newStandaloneWorld(t, func(c map[string]any) { c["Console"] = true })
		var issuer string
		metadata := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/.well-known/openid-configuration" {
				http.NotFound(rw, r)
				return
			}
			rw.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(rw).Encode(map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/oauth2/authorize", "token_endpoint": issuer + "/oauth2/token"})
		}))
		t.Cleanup(metadata.Close)
		issuer = metadata.URL
		_, signer, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		der, err := x509.MarshalPKIXPublicKey(signer.Public())
		require.NoError(t, err)
		app := iam.RemoteApplication{Issuer: issuer, Mode: iam.RemoteApplicationModeStatic, PublicKeys: []iam.RemoteApplicationKey{{KID: "app", PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}}
		body, err := json.Marshal(app)
		require.NoError(t, err)
		var stored iam.RemoteApplication
		w.command(t, map[string]any{"Op": "trust-application", "Merchant": w.Merchants["beta"].ID.String(), "Body": json.RawMessage(body)}, &stored)
		status, raw := w.request(t, http.MethodGet, "/admin/config.json", "", nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var cfg struct {
			Issuers  []struct{ Key, URL, ClientID, Name, Resource, Scope string } `json:"issuers"`
			Exchange *struct {
				ClientID string `json:"client_id"`
			} `json:"exchange"`
			LocalSignIn *bool `json:"local_sign_in"`
		}
		require.NoError(t, json.Unmarshal(raw, &cfg))
		// beta's application signs people in; alpha's and beta's assertion
		// signers do not.
		require.Len(t, cfg.Issuers, 1, string(raw))
		got := cfg.Issuers[0]
		require.Equal(t, issuer, got.URL)
		require.Equal(t, "beta", got.Name)
		require.Equal(t, w.URL, got.Resource)
		require.Equal(t, "openid profile email openrails:merchant", got.Scope)
		require.Contains(t, string(raw), `"client_id":"openrails-console"`)
		require.NotNil(t, cfg.Exchange, "the server's own accounts always sign staff in")
		require.Equal(t, "openrails-console", cfg.Exchange.ClientID)
		require.Nil(t, cfg.LocalSignIn)
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

	t.Run("OR-J06-S73/auth-settings-beyond-the-key-path-refuse-boot", func(t *testing.T) {
		w := newWorld(t)
		const derived = "OpenRails derives the server's AuthKit configuration; only auth.keys.path (AUTH_KEYS_PATH) is set"
		for _, c := range []struct {
			name  string
			extra map[string]any
			env   string
			says  []string
		}{
			{"key", map[string]any{"auth": map[string]any{"password": map[string]any{"min_length": 12}}}, "", []string{"auth.password.min_length (AUTH_PASSWORD_MIN_LENGTH)", derived}},
			{"env", nil, "AUTH_TOKEN_ISSUER=https://billing.critical.invalid", []string{"auth.token.issuer (AUTH_TOKEN_ISSUER): set public_billing_base_url", derived}},
			{"private", nil, "AUTH_TOKEN_ALLOW_PRIVATE_NETWORK_JWKS=true", []string{"auth.token.allow_private_network_jwks (AUTH_TOKEN_ALLOW_PRIVATE_NETWORK_JWKS)", derived}},
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

	t.Run("OR-J06-S74/a-server-whose-staff-cannot-enroll-a-second-factor-is-not-ready", func(t *testing.T) {
		w := newWorld(t)
		t.Cleanup(func() {
			_, _ = w.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{w.schema + "_auth"}.Sanitize()+" CASCADE")
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
			cmd := hostCLICommand(t, w, map[string]any{"host": "127.0.0.1", "port": port, "public_billing_base_url": "https://billing.critical.invalid", "auth": map[string]any{"keys": map[string]any{"path": keys}}}, "run-server")
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
