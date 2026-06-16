package solana

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// fakeTransit emulates Vault Transit by holding a real Ed25519 key in-test and
// signing with it — but the transitSigner only ever sees the TransitClient
// interface, never the key, exactly like the real Vault boundary.
type fakeTransit struct {
	mu       sync.Mutex
	key      solanago.PrivateKey
	signs    int
	pubReads int
	err      error
}

func (f *fakeTransit) Sign(_ context.Context, _ string, input []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signs++
	if f.err != nil {
		return nil, f.err
	}
	sig, err := f.key.Sign(input)
	if err != nil {
		return nil, err
	}
	return sig[:], nil
}

func (f *fakeTransit) PublicKey(_ context.Context, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pubReads++
	if f.err != nil {
		return nil, f.err
	}
	pk := f.key.PublicKey()
	return pk[:], nil
}

func TestTransitSignerSignsAndVerifies(t *testing.T) {
	key := newTestKey(t)
	ft := &fakeTransit{key: key}
	signer := NewTransitSigner(ft, nil, time.Minute)
	ctx := context.Background()
	tid := dbtest.TestMerchantID

	pub, err := signer.PublicKey(ctx, tid)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if !pub.Equals(key.PublicKey()) {
		t.Fatalf("transit public key mismatch: %s vs %s", pub, key.PublicKey())
	}

	msg := []byte("transfer_subscription message bytes")
	sig, err := signer.SignMessage(ctx, tid, msg)
	if err != nil {
		t.Fatalf("SignMessage: %v", err)
	}
	if !sig.Verify(pub, msg) {
		t.Fatal("transit signature did not verify")
	}
}

func TestTransitSignerCachesPublicKeyNotSignatures(t *testing.T) {
	key := newTestKey(t)
	ft := &fakeTransit{key: key}
	signer := NewTransitSigner(ft, nil, time.Minute)
	ctx := context.Background()
	tid := dbtest.TestMerchantID

	for range 3 {
		if _, err := signer.PublicKey(ctx, tid); err != nil {
			t.Fatalf("PublicKey: %v", err)
		}
	}
	for range 3 {
		if _, err := signer.SignMessage(ctx, tid, []byte("m")); err != nil {
			t.Fatalf("SignMessage: %v", err)
		}
	}

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.pubReads != 1 {
		t.Errorf("public key reads = %d, want 1 (cached)", ft.pubReads)
	}
	if ft.signs != 3 {
		t.Errorf("signs = %d, want 3 (never cached — every signature round-trips Vault)", ft.signs)
	}
}

func TestTransitSignerFailClosed(t *testing.T) {
	ft := &fakeTransit{key: newTestKey(t), err: errors.New("vault transit unreachable")}
	signer := NewTransitSigner(ft, nil, time.Minute)
	if _, err := signer.SignMessage(context.Background(), dbtest.TestMerchantID, []byte("m")); err == nil {
		t.Fatal("expected error when transit fails, got nil")
	}
}

func TestTransitSignerCustomKeyName(t *testing.T) {
	var gotName string
	ft := &fakeTransit{key: newTestKey(t)}
	naming := func(id merchant.ID) string { gotName = "custom-" + id.String(); return gotName }
	signer := NewTransitSigner(ft, naming, time.Minute)
	if _, err := signer.PublicKey(context.Background(), dbtest.TestMerchantID); err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if gotName == "" {
		t.Fatal("custom key-name function was not used")
	}
}
