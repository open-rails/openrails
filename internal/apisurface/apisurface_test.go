package apisurface

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// methodTypes are the aliased internal types whose methods are what they are:
// an error, and the card value that redacts itself wherever it is printed or
// encoded. Every other method on an aliased type is an engine helper and
// belongs in a function.
var methodTypes = []string{
	"openrails: (GateError) ",
	"openrails/billing: (Card) ", "openrails/billing: (*Card) ",
}

// TestGoAPISurface keeps api/go.txt equal to the exports of the public
// packages, so a change to the Go API is deliberate: regenerate with
// go run ./scripts/contracts -write and review the diff. The public API may
// reach an internal type only through an alias a public package declares, and
// such an alias brings no helper methods.
func TestGoAPISurface(t *testing.T) {
	s, err := Load(t.Context(), filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Leaks) > 0 {
		t.Errorf("the public API exposes internal types no public package names:\n%s", strings.Join(s.Leaks, "\n"))
	}
	for _, m := range s.AliasMethods {
		if !slices.ContainsFunc(methodTypes, func(prefix string) bool { return strings.HasPrefix(m, prefix) }) {
			t.Errorf("aliased internal type exports a method: %s", m)
		}
	}
	file, err := os.ReadFile(filepath.Join("..", "..", File))
	if err != nil {
		t.Fatal(err)
	}
	listed := strings.Split(strings.TrimSuffix(string(file), "\n"), "\n")
	missing := func(from, in []string) []string {
		var out []string
		for _, line := range from {
			if _, ok := slices.BinarySearch(in, line); !ok {
				out = append(out, line)
			}
		}
		return out
	}
	if gone := missing(listed, s.Features); len(gone) > 0 {
		t.Errorf("removed or changed (%v):\n%s", ErrStale, strings.Join(gone, "\n"))
	}
	if added := missing(s.Features, listed); len(added) > 0 {
		t.Errorf("added (%v):\n%s", ErrStale, strings.Join(added, "\n"))
	}
	if string(s.Text()) != string(file) {
		t.Error(ErrStale)
	}
}
