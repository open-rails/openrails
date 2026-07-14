package basistheory

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func signPSS(t *testing.T, key *rsa.PrivateKey, body []byte) string {
	t.Helper()
	digest := sha256.Sum256(body)
	sig, err := rsa.SignPSS(rand.Reader, key, crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func serveKey(t *testing.T, pub *rsa.PublicKey) *httptest.Server {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pemBytes)
	}))
}

// TestWebhookVerifier pins RSA-PSS SHA-256 verification against a locally
// generated keypair served from an injected key URL (mirrors the CDN contract).
func TestWebhookVerifier(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := serveKey(t, &key.PublicKey)
	defer srv.Close()

	body := []byte(`{"id":"evt_1","tenant_id":"tnt_1","type":"token.deleted","data":{"token":{"id":"tok_1"}}}`)
	v := NewWebhookVerifier(srv.URL)

	t.Run("valid signature verifies", func(t *testing.T) {
		if err := v.Verify(body, signPSS(t, key, body), "v1"); err != nil {
			t.Fatalf("verify: %v", err)
		}
	})

	t.Run("tampered body fails", func(t *testing.T) {
		tampered := append([]byte(nil), body...)
		tampered[len(tampered)-2] = 'X'
		if err := v.Verify(tampered, signPSS(t, key, body), "v1"); !errors.Is(err, ErrWebhookSignatureInvalid) {
			t.Fatalf("want invalid signature, got %v", err)
		}
	})

	t.Run("wrong version fails loudly", func(t *testing.T) {
		if err := v.Verify(body, signPSS(t, key, body), "v9"); !errors.Is(err, ErrWebhookVersionUnknown) {
			t.Fatalf("want version error, got %v", err)
		}
	})

	t.Run("missing signature fails", func(t *testing.T) {
		if err := v.Verify(body, "", "v1"); !errors.Is(err, ErrWebhookSignatureMissing) {
			t.Fatalf("want missing signature, got %v", err)
		}
	})

	t.Run("wrong key fails", func(t *testing.T) {
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Verify(body, signPSS(t, other, body), "v1"); !errors.Is(err, ErrWebhookSignatureInvalid) {
			t.Fatalf("want invalid signature, got %v", err)
		}
	})
}

// TestWebhookVerifierKeyRotation: a rotated CDN key self-heals on the next
// verification (refetch-once-on-mismatch).
func TestWebhookVerifierKeyRotation(t *testing.T) {
	oldKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	newKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	current := &oldKey.PublicKey
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		der, _ := x509.MarshalPKIXPublicKey(current)
		_, _ = w.Write(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	}))
	defer srv.Close()

	v := NewWebhookVerifier(srv.URL)
	body := []byte(`{"id":"evt_2"}`)
	if err := v.Verify(body, signPSS(t, oldKey, body), "v1"); err != nil {
		t.Fatalf("initial verify: %v", err)
	}
	current = &newKey.PublicKey // CDN rotates
	if err := v.Verify(body, signPSS(t, newKey, body), "v1"); err != nil {
		t.Fatalf("post-rotation verify should self-heal: %v", err)
	}
}
