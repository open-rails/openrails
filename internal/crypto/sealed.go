// Package crypto seals short-lived runtime secrets at rest under a key derived
// from a credential the merchant's configuration already holds, so no master
// key is configured and no data key is stored.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// AAD binds a ciphertext to the row it belongs to: a blob moved elsewhere
// fails to open.
type AAD []byte

// SecretAAD is the binding for (merchant, name), length-prefixed so no two
// distinct pairs encode to the same bytes.
func SecretAAD(merchantID billing.MerchantID, name string) AAD {
	id := merchantID.UUID()
	out := make([]byte, 0, 32+len(id)+len(name))
	out = append(out, "openrails/secret/v1\x00"...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(id)))
	out = append(out, id[:]...)
	n := len(name)
	if n > math.MaxUint32 {
		n = math.MaxUint32
	}
	out = binary.BigEndian.AppendUint32(out, uint32(n))
	out = append(out, name...)
	return out
}

// Sealer seals with AES-256-GCM under a key derived from a credential.
type Sealer struct{ gcm cipher.AEAD }

// NewSealer derives the key from credential for purpose (HKDF-SHA256); a new
// credential cannot open what the old one sealed.
func NewSealer(credential, purpose string) (*Sealer, error) {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return nil, errors.New("crypto: sealing needs a credential")
	}
	key, err := hkdf.Key(sha256.New, []byte(credential), nil, "openrails/"+purpose, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{gcm: gcm}, nil
}

// Seal returns base64(nonce || ciphertext || tag).
func (s *Sealer) Seal(aad AAD, plaintext []byte) (string, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(s.gcm.Seal(nonce, nonce, plaintext, aad)), nil
}

// Open reverses Seal against the same aad.
func (s *Sealer) Open(aad AAD, sealed string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, err
	}
	if len(raw) < s.gcm.NonceSize() {
		return nil, errors.New("crypto: ciphertext too short")
	}
	nonce, body := raw[:s.gcm.NonceSize()], raw[s.gcm.NonceSize():]
	return s.gcm.Open(nil, nonce, body, aad)
}
