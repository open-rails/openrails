package stripeapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/providerposture"
)

// WriteDisarmed answers what the transport would, without asking Stripe: a
// live key never writes to an injected provider, a test key writes there,
// and through Stripe a key writes unless no loaded PSP declares it or its
// recorded verdict refuses it; one still being checked is left to the gate.
func TestWriteDisarmedFollowsTheTransportGate(t *testing.T) {
	fake := NewFactory(http.DefaultTransport)
	if err := fake.WriteDisarmed("sk_live_fixture"); !errors.Is(err, providerposture.ErrDisarmed) {
		t.Fatalf("a live key to a fake provider: %v", err)
	}
	if err := fake.WriteDisarmed("sk_test_fixture"); err != nil {
		t.Fatalf("a test key to a fake provider: %v", err)
	}
	direct := NewFactory(nil)
	name := strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := direct.WriteDisarmed("sk_test_undeclared" + name); !errors.Is(err, providerposture.ErrPending) {
		t.Fatalf("a key no loaded PSP declares: %v", err)
	}
	checking, release := "sk_test_checking"+name, make(chan struct{})
	defer close(release)
	providerposture.Process().Register(PostureKey(uuid.New(), uuid.New(), "acct_checking", checking), func(context.Context) (providerposture.Verdict, error) {
		<-release
		return providerposture.Simulated, nil
	})
	if err := direct.WriteDisarmed(checking); err != nil {
		t.Fatalf("a key still being checked is left to its write's gate: %v", err)
	}
	refused := "sk_test_refused" + name
	key := PostureKey(uuid.New(), uuid.New(), "acct_declared", refused)
	providerposture.Process().Verify(context.Background(), key, func(context.Context) (providerposture.Verdict, error) {
		return providerposture.Mismatched, errors.New("the key belongs to another account")
	})
	if err := direct.WriteDisarmed(refused); !errors.Is(err, providerposture.ErrDisarmed) {
		t.Fatalf("a key whose verdict refuses writes: %v", err)
	}
}
