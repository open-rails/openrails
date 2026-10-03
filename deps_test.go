package openrails

import (
	"os/exec"
	"strings"
	"testing"
)

// TestCorePackagesStayFrameworkNeutral keeps framework implementations inside
// their opt-in adapters. A shared module does not make them engine dependencies.
func TestCorePackagesStayFrameworkNeutral(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".", "./internal/config", "./internal/engine", "./internal/billingauth", "./adapters/http").CombinedOutput()
	if err != nil {
		t.Fatalf("list core package dependencies: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, framework := range []string{"github.com/gin-gonic/gin", "github.com/gofiber/fiber"} {
			if dep == framework || strings.HasPrefix(dep, framework+"/") {
				t.Errorf("core package links opt-in framework dependency %q", dep)
			}
		}
	}
}
