package contractaudit

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestPublicInternalTypeMutationsDrift(t *testing.T) {
	base := repositoryFS(t)
	c := newCapturer()
	snapshot, err := c.capture(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, before, after string }{
		{"internal/operator/controlplane.go", "PasswordlessLogin            bool", "PasswordlessLogin            string"},
		{"internal/reconcile/merchant_wiring.go", "StripeBaseURL         string", "StripeBaseURL         bool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := fs.ReadFile(base, tc.name)
			if err != nil {
				t.Fatal(err)
			}
			changed := bytes.Replace(raw, []byte(tc.before), []byte(tc.after), 1)
			if bytes.Equal(raw, changed) {
				t.Fatal("mutation target not found")
			}
			err = c.verify(overlayFS{base, map[string][]byte{SnapshotPath: snapshot, tc.name: changed}})
			if !errors.Is(err, ErrContractDrift) {
				t.Fatalf("public internal type changed without contract drift: %v", err)
			}
		})
	}
}

func TestReachableInternalTypesDoNotFreezeImplementation(t *testing.T) {
	base := fstest.MapFS{
		"api.go": {Data: []byte(`package public
import impl "github.com/open-rails/openrails/internal/implementation"
type Alias = impl.Node
type Generic = impl.Box[string]
type Config struct { Endpoint impl.Endpoint }
func New() *impl.Node { return nil }
`)},
		"internal/implementation/types.go": {Data: []byte(`package implementation
type Node struct { Next *Node; Value string; private privateState }
func (*Node) Read() Detail { return Detail{} }
type Detail struct { Count int }
type Endpoint struct { Address string }
type privateState struct { Secret string }
type Unrelated struct { Field string }
type T struct { Shadowed string }
type Box[T any] struct { Value T }
func (Box[T]) Echo(v T) T { return v }
`)},
	}
	c := newCapturer()
	snapshot, err := c.capture(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		before, after string
		drift         bool
	}{
		{"Value string", "Value bool", true},
		{"Count int", "Count int64", true},
		{"Address string", "Address bool", true},
		{"Read() Detail", "Read(int) Detail", true},
		{"private privateState", "private string", false},
		{"Secret string", "Secret bool", false},
		{"Field string", "Field bool", false},
		{"Shadowed string", "Shadowed bool", false},
		{"return Detail{}", "panic(\"implementation only\")", false},
	} {
		t.Run(tc.before, func(t *testing.T) {
			const name = "internal/implementation/types.go"
			raw := base[name].Data
			changed := bytes.Replace(raw, []byte(tc.before), []byte(tc.after), 1)
			if bytes.Equal(raw, changed) {
				t.Fatal("mutation target not found")
			}
			err := c.verify(overlayFS{base, map[string][]byte{SnapshotPath: snapshot, name: changed}})
			if errors.Is(err, ErrContractDrift) != tc.drift {
				t.Fatalf("want drift=%v, got %v", tc.drift, err)
			}
		})
	}
}
