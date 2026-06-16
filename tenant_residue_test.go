package openrails

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoActiveTenantVocabularyResidue(t *testing.T) {
	forbiddenText := []string{
		"tenant_secrets",
		"tenant_deks",
		"tenant_exports",
		"tenant_processor",
		"openrails.tenants",
		"openrails.tenant",
		"billing.tenants",
		"billing.tenant",
		"tenant_cors",
		"TenantCORS",
		"TenantSlug",
		"ResolveTenant",
		"TenantSecretGetter",
		"TenantSecretStore",
		"TenantSecrets",
		"tenant-admin",
		"/v1/admin/tenants",
		"/v1/t/",
		"/v1/service/tenant",
		"openrails/tenants",
		"<tenant-slug>",
		"tenant_writable",
	}
	forbiddenPath := []string{
		"internal/tenancy",
		"tenant_processor",
		"tenant_rls",
		"tenant_provisioning",
		"tenant_aware",
		"admin_metrics_tenant",
		"tenant_manifest",
	}

	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		path = filepath.ToSlash(path)
		if entry.IsDir() {
			switch path {
			case ".git", ".cache", ".codegraph", ".task", "agents", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !activeResidueFile(path) {
			return nil
		}
		for _, fragment := range forbiddenPath {
			if strings.Contains(path, fragment) {
				t.Errorf("%s: stale tenant path fragment %q", path, fragment)
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, fragment := range forbiddenText {
			if strings.Contains(text, fragment) {
				t.Errorf("%s: stale tenant text fragment %q", path, fragment)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func activeResidueFile(path string) bool {
	switch path {
	case "tenant_residue_test.go", "docs/tenant-subject-hardcut-map.md", "docs/solana-subscriptions-plan.md":
		return false
	}
	for _, suffix := range []string{
		".go",
		".md",
		".sql",
		".yaml",
		".yml",
		".json",
		".env.example",
	} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}
