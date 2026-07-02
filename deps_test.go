package openrails

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestRootPackageStaysLight enforces the #338 package-layout contract: the root
// openrails package (interface + remote client) must NOT pull the engine, so a
// remote-only consumer's binary does not link pgx/river/gin or any internal
// package. The heavy engine lives exclusively in openrails/embed.
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
		"github.com/jackc/pgx",
		"github.com/riverqueue/river",
		"github.com/redis/go-redis",
		"github.com/open-rails/authkit",
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep := strings.TrimSpace(line)
		if dep == "github.com/open-rails/openrails/pkg/merchant" {
			continue
		}
		for _, bad := range forbidden {
			if dep == bad || strings.HasPrefix(dep, bad+"/") {
				t.Errorf("root openrails package links engine dependency %q — keep the root remote-only light (#338)", dep)
			}
		}
	}
}

// TestModuleIsGinFree pins the #670 gin exit for the WHOLE module: gin must not
// reappear in go.mod (direct or indirect — post-1.17 go.mod lists both). The
// HTTP surface is framework-neutral net/http; hosts wrap it themselves
// (gin.WrapH etc.).
func TestModuleIsGinFree(t *testing.T) {
	gomod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	if strings.Contains(string(gomod), "github.com/gin-gonic/gin") {
		t.Errorf("github.com/gin-gonic/gin is back in go.mod — the module is gin-free since #670; mount the neutral handler via the host framework's WrapH instead")
	}
}
