package openrails

import (
	"os/exec"
	"strings"
	"testing"
)

// TestRootPackageStaysLight enforces the #338 package-layout contract: the root
// openrails package (interface + remote client) must NOT pull the engine, so a
// remote-only consumer's binary does not link pgx/river/gin or an internal
// engine package. The archive wire verifier is standard-library-only; the
// catalog and configdocument packages contain only declarations and bounded
// parsing/validation (no engine dependencies); the
// heavy engine lives exclusively in openrails/embed.
func TestRootPackageStaysLight(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}
	out, err := exec.Command(goBin, "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps .: %v\n%s", err, out)
	}

	forbidden := []string{
		"github.com/open-rails/openrails/internal",
		"github.com/open-rails/openrails/pkg",
		"github.com/open-rails/openrails/embed",
		"github.com/gin-gonic/gin",
		"github.com/gofiber/fiber",
		"github.com/jackc/pgx",
		"github.com/riverqueue/river",
		"github.com/redis/go-redis",
		"github.com/open-rails/authkit",
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep := strings.TrimSpace(line)
		if dep == "github.com/open-rails/openrails/pkg/catalog" || dep == "github.com/open-rails/openrails/pkg/merchant" || dep == "github.com/open-rails/openrails/pkg/pricing" || dep == "github.com/open-rails/openrails/internal/archivewire" || dep == "github.com/open-rails/openrails/internal/configdocument" {
			continue
		}
		for _, bad := range forbidden {
			if dep == bad || strings.HasPrefix(dep, bad+"/") {
				t.Errorf("root openrails package links engine dependency %q — keep the root remote-only light (#338)", dep)
			}
		}
	}
}

// TestCorePackagesStayFrameworkNeutral keeps framework implementations inside
// their opt-in adapters. A shared module does not make them engine dependencies.
func TestCorePackagesStayFrameworkNeutral(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./config", "./embed", "./pkg/billingauth", "./adapters/http").CombinedOutput()
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
