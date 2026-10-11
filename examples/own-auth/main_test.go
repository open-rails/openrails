package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The example, its tests included, builds on helpers/auth alone: no AuthKit
// package is among its dependencies.
func TestOwnAuthImportsNoAuthKit(t *testing.T) {
	list := exec.Command("go", "list", "-deps", "-test", "-tags", "e2e,integration", "-f", "{{.ImportPath}}", ".")
	list.Env = append(os.Environ(), "GOWORK=off")
	out, err := list.CombinedOutput()
	require.NoError(t, err, string(out))
	deps := strings.Fields(string(out))
	require.Contains(t, deps, "github.com/open-rails/helpers/auth")
	for _, dep := range deps {
		require.False(t, strings.HasPrefix(dep, "github.com/open-rails/authkit"), "own-auth depends on %s", dep)
	}
}
