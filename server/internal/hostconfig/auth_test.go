package hostconfig

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/require"
)

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
