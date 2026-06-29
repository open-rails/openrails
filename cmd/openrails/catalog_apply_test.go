package main

import (
	"testing"
)

func TestPushCatalogCommandShape(t *testing.T) {
	cmd := newPushCatalogCmd()
	if cmd.Use != "push-merchant-catalog" {
		t.Fatalf("Use = %q", cmd.Use)
	}
	if len(cmd.Aliases) != 1 || cmd.Aliases[0] != "push-catalog" {
		t.Fatalf("Aliases = %#v", cmd.Aliases)
	}
	for _, name := range []string{"file", "insert", "overwrite", "prune"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing flag %q", name)
		}
	}
}
