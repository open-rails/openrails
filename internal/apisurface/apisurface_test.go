package apisurface

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestGoAPISurface keeps api/go.txt equal to the exports of the public
// packages, so a change to the Go API is deliberate: regenerate with
// go run ./scripts/contracts -write and review the diff.
func TestGoAPISurface(t *testing.T) {
	s, err := Load()
	if err != nil {
		t.Fatal(err)
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
