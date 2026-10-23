//go:build e2e && integration

package ci_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	authkitKeys "github.com/open-rails/authkit/keys"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/server"
)

// A user-verifying passkey remains an enrollable second factor while code
// factors are unavailable. Required-MFA accounts still cannot acquire a full
// identity before enrollment. Development keys persist their TOTP key.
func TestControlPlaneRequiresAnEnrollableSecondFactor(t *testing.T) {
	f := newFixture(t)
	attach := func(keys authkit.KeysConfig, source authkitKeys.Source) (*server.Server, error) {
		t.Helper()
		return f.buildServer(t, func(cfg *server.Config, deps *server.Deps) {
			cfg.Auth.Keys = keys
			cfg.Auth.TwoFactor.Mode = iam.TwoFactorRequired
			deps.Auth.KeySource = source
		})
	}
	methods := func(cp *server.Server) []string {
		t.Helper()
		handler, err := standaloneHandler(cp)
		require.NoError(t, err)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+f.schema+"/v1/capabilities", nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var caps struct {
			TwoFactor struct{ Methods []string } `json:"two_factor"`
			Passkeys  struct{ Login bool }       `json:"passkeys"`
		}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&caps))
		require.True(t, caps.Passkeys.Login, "a user-verifying passkey remains available without code factors")
		return caps.TwoFactor.Methods
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keys := t.TempDir()
	signing, err := authkitKeys.StaticFromPEM("e2e", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), nil)
	require.NoError(t, err)
	cp, err := attach(authkit.KeysConfig{Path: keys}, signing)
	require.NoError(t, err)
	require.Empty(t, methods(cp))
	require.NoError(t, authProbe(t, cp, "authkit_two_factor"), "a user-verifying passkey can still satisfy mandatory MFA")
	u := newAccount(t, cp)
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]string{"identifier": u.Email, "password": u.Password})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/"+f.schema+"/v1/password/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, request)
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	var result struct {
		Status     string `json:"status"`
		TokenSet   any    `json:"token_set"`
		Enrollment struct {
			TokenSet struct {
				AccessToken string `json:"access_token"`
			} `json:"token_set"`
			AllowedMethods []string `json:"allowed_methods"`
		} `json:"enrollment"`
	}
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &result))
	require.Equal(t, "enrollment_required", result.Status)
	require.Nil(t, result.TokenSet, "no authenticated session before the required second factor")
	require.Empty(t, result.Enrollment.AllowedMethods)
	require.NotEmpty(t, result.Enrollment.TokenSet.AccessToken)
	_, err = cp.AuthKit().Verify(t.Context(), result.Enrollment.TokenSet.AccessToken)
	require.Error(t, err, "an enrollment-only token is not a full identity")

	totp := make([]byte, 32)
	_, _ = rand.Read(totp)
	require.NoError(t, os.WriteFile(filepath.Join(keys, "totp.key"), []byte(hex.EncodeToString(totp)), 0o600))
	cp, err = attach(authkit.KeysConfig{Path: keys}, signing)
	require.NoError(t, err, "totp.key beside the signing keys")
	require.Contains(t, methods(cp), "totp")

	dev := t.TempDir()
	cp, err = attach(authkit.KeysConfig{Path: dev, AllowEphemeralDevKeys: true}, nil)
	require.NoError(t, err, "a disposable TOTP key beside a disposable signing key")
	require.Contains(t, methods(cp), "totp")
	require.FileExists(t, filepath.Join(dev, "totp.key"))
}

func authProbe(t *testing.T, cp *server.Server, name string) error {
	t.Helper()
	for _, probe := range cp.AuthKit().Probes() {
		if probe.Name == name {
			return probe.Check(context.Background())
		}
	}
	t.Fatalf("AuthKit probe %s is missing", name)
	return nil
}
