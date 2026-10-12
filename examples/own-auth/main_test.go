package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The example, its tests included, uses AuthKit's contracts without its runtime.
func TestOwnAuthImportsOnlyAuthKitContracts(t *testing.T) {
	list := exec.Command("go", "list", "-deps", "-test", "-tags", "e2e,integration", "-f", "{{.ImportPath}}", ".")
	list.Env = append(os.Environ(), "GOWORK=off")
	out, err := list.CombinedOutput()
	require.NoError(t, err, string(out))
	deps := strings.Fields(string(out))
	require.Contains(t, deps, "github.com/open-rails/authkit/auth")
	for _, dep := range deps {
		if strings.HasPrefix(dep, "github.com/open-rails/authkit") {
			require.Contains(t, []string{"github.com/open-rails/authkit/auth", "github.com/open-rails/authkit/auth/authtest", "github.com/open-rails/authkit/userinfo", "github.com/open-rails/authkit/userinfo/userinfotest"}, dep, "own-auth imports AuthKit implementation %s", dep)
		}
	}
}
