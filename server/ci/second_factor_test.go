//go:build e2e && integration

package ci_test

import (
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
	authkitKeys "github.com/open-rails/authkit/keys"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/server"
)

// The root owner always needs a second factor, so a control plane where none
// can be enrolled refuses to start: no totp.key beside the signing keys and no
// sender. A deployment allowed a disposable signing key gets a TOTP key too,
// which AuthKit writes beside it so restarts keep enrollments.
func TestControlPlaneRequiresAnEnrollableSecondFactor(t *testing.T) {
	f := newFixture(t)
	attach := func(keys authkit.KeysConfig, source authkitKeys.Source) (*server.Server, error) {
		t.Helper()
		return f.buildServer(t, func(cfg *server.Config, deps *server.Deps) {
			cfg.Auth.Keys = keys
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
		}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&caps))
		return caps.TwoFactor.Methods
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keys := t.TempDir()
	signing, err := authkitKeys.StaticFromPEM("e2e", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), nil)
	require.NoError(t, err)
	_, err = attach(authkit.KeysConfig{Path: keys}, signing)
	require.ErrorContains(t, err, "no second factor can be enrolled")

	totp := make([]byte, 32)
	_, _ = rand.Read(totp)
	require.NoError(t, os.WriteFile(filepath.Join(keys, "totp.key"), []byte(hex.EncodeToString(totp)), 0o600))
	cp, err := attach(authkit.KeysConfig{Path: keys}, signing)
	require.NoError(t, err, "totp.key beside the signing keys")
	require.Contains(t, methods(cp), "totp")

	dev := t.TempDir()
	cp, err = attach(authkit.KeysConfig{Path: dev, AllowEphemeralDevKeys: true}, nil)
	require.NoError(t, err, "a disposable TOTP key beside a disposable signing key")
	require.Contains(t, methods(cp), "totp")
	require.FileExists(t, filepath.Join(dev, "totp.key"))
}
