package hostconfig

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultTenantConfiguration(t *testing.T) {
	bootEnv(t)
	const fileTenant = "7a3d15bc-f4e0-438b-a8a0-877172ec77ee"
	const envTenant = "c9c71f96-a9be-4b86-b3a4-d60fd2e71374"
	path := writeFile(t, filepath.Join(t.TempDir(), "tenant.yaml"), "auth:\n  default_tenant_id: "+fileTenant+"\n")
	cfg, err := LoadForCommand(path)
	require.NoError(t, err)
	require.Equal(t, fileTenant, cfg.Auth.DefaultTenantID.String())
	t.Setenv("AUTH_DEFAULT_TENANT_ID", envTenant)
	cfg, err = LoadForCommand(path)
	require.NoError(t, err)
	require.Equal(t, envTenant, cfg.Auth.DefaultTenantID.String())
	t.Setenv("AUTH_DEFAULT_TENANT_ID", "invalid")
	_, err = LoadForCommand(path)
	require.ErrorContains(t, err, "default_tenant_id")
}

// An inline signing key is AuthKit's key source; without one AuthKit reads
// auth.keys.path.
func TestSigningKeySource(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	src, err := SigningKey{ActiveKeyID: "k", ActivePrivateKeyPEM: keyPEM}.Source()
	require.NoError(t, err)
	require.Equal(t, "k", src.ActiveSigner().KID())
	_, err = SigningKey{ActiveKeyID: "k", ActivePrivateKeyPEM: keyPEM, PublicKeysJSON: "{"}.Source()
	require.ErrorContains(t, err, "AUTHKIT_PUBLIC_KEYS")
	_, err = SigningKey{ActiveKeyID: "k", ActivePrivateKeyPEM: "not a pem"}.Source()
	require.Error(t, err)
	src, err = SigningKey{}.Source()
	require.NoError(t, err)
	require.Nil(t, src)
}
