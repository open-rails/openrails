package basistheory

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestWebhookKeyRefetchIsBounded(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	var garbage atomic.Bool
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		if garbage.Load() {
			_, _ = w.Write([]byte("not a key"))
			return
		}
		_ = pem.Encode(w, &pem.Block{Type: "PUBLIC KEY", Bytes: der})
	}))
	defer cdn.Close()
	body := []byte(`{"type":"token.created"}`)
	digest := sha256.Sum256(body)
	signed, err := rsa.SignPSS(rand.Reader, key, crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthAuto, Hash: crypto.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	good := base64.StdEncoding.EncodeToString(signed)
	forged := base64.StdEncoding.EncodeToString(make([]byte, len(signed)))
	v := NewWebhookVerifier(cdn.URL)
	if err := v.Verify(context.Background(), body, good, "v1"); err != nil || fetches.Load() != 1 {
		t.Fatalf("first delivery fetches the key once: %v, %d fetches", err, fetches.Load())
	}

	v.RefetchEvery = 1 // every forged signature may ask; only one fetch runs at a time
	garbage.Store(true)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := v.Verify(context.Background(), body, forged, "v1"); err == nil {
				t.Error("a forged signature verified")
			}
		}()
	}
	wg.Wait()
	if err := v.Verify(context.Background(), body, good, "v1"); err != nil {
		t.Fatalf("a key that does not parse leaves the cached one: %v", err)
	}

	before := fetches.Load()
	v.RefetchEvery = 0 // the default window
	for range 20 {
		_ = v.Verify(context.Background(), body, forged, "v1")
	}
	if got := fetches.Load() - before; got > 1 {
		t.Fatalf("forged signatures refetched the key %d times inside one window", got)
	}
}
