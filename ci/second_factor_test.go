//go:build e2e && integration

package ci_test

import (
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

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/standalonedb"
)

// The root owner always needs a second factor, so a control plane where none
// can be enrolled refuses to start (AuthKit #419): no totp.key beside the
// signing keys and no sender. A deployment allowed a disposable signing key
// gets a TOTP key too, which AuthKit writes beside it so restarts keep
// enrollments.
func TestControlPlaneRequiresAnEnrollableSecondFactor(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, standalonedb.ApplyAuthKit(t.Context(), f.pool))
	attach := func(auth openrails.AuthConfig) (*openrails.Client, error) {
		t.Helper()
		auth.Issuer = "http://127.0.0.1/" + f.schema
		auth.AllowMemory, auth.AllowMissingSenders, auth.AllowLoopbackHTTP, auth.DirectPeerIP = true, true, true, true
		cfg := f.config()
		cfg.AllowCatalogUpdates = false
		cfg.ControlPlane = &openrails.ControlPlaneConfig{Auth: auth}
		client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
		if err == nil {
			t.Cleanup(func() { _ = client.Close(context.Background()) })
		}
		return client, err
	}
	methods := func(cp *openrails.Client) []string {
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
	signing := config.AuthConfig{
		ActiveKeyID:         "e2e",
		ActivePrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		KeysPath:            keys,
	}
	_, err = attach(signing)
	require.ErrorContains(t, err, "no second factor can be enrolled")

	totp := make([]byte, 32)
	_, _ = rand.Read(totp)
	require.NoError(t, os.WriteFile(filepath.Join(keys, "totp.key"), []byte(hex.EncodeToString(totp)), 0o600))
	cp, err := attach(signing)
	require.NoError(t, err, "totp.key beside the signing keys")
	require.Contains(t, methods(cp), "totp")

	dev := t.TempDir()
	cp, err = attach(config.AuthConfig{AllowEphemeralSigningKey: true, KeysPath: dev})
	require.NoError(t, err, "a disposable TOTP key beside a disposable signing key")
	require.Contains(t, methods(cp), "totp")
	require.FileExists(t, filepath.Join(dev, "totp.key"))
}
