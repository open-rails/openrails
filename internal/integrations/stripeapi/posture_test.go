package stripeapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/open-rails/openrails/internal/providerposture"
)

// WriteDisarmed answers what the transport would: a live key never writes to
// an injected provider, and a test key writes unless its recorded verdict
// refuses it.
func TestWriteDisarmedFollowsTheTransportGate(t *testing.T) {
	fake := NewFactory(http.DefaultTransport)
	if err := fake.WriteDisarmed("sk_live_fixture"); !errors.Is(err, providerposture.ErrDisarmed) {
		t.Fatalf("a live key to a fake provider: %v", err)
	}
	if err := fake.WriteDisarmed("sk_test_fixture"); err != nil {
		t.Fatalf("a test key to a fake provider: %v", err)
	}
	direct := NewFactory(nil)
	if err := direct.WriteDisarmed("sk_test_unchecked"); err != nil {
		t.Fatalf("a key not checked yet is left to its write's gate: %v", err)
	}
	providerposture.Process().Verify(context.Background(), PostureKey("sk_test_refused", APIBase, ""), func(context.Context) (providerposture.Verdict, error) {
		return providerposture.Mismatched, errors.New("the key belongs to another account")
	})
	if err := direct.WriteDisarmed("sk_test_refused"); !errors.Is(err, providerposture.ErrDisarmed) {
		t.Fatalf("a test key whose verdict refuses writes: %v", err)
	}
}
