package merchants

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Path-doctrine guard (#724, custodial-merchant-secrets ADR):
// merchant-secret isolation rests on ALL secret paths being derived from the
// single PSPSecretName builder and the single vaultSecretStore
// namespace builder. Ad-hoc construction anywhere else would silently escape the
// per-merchant namespace, so this test greps the production sources and fails on
// any non-allowlisted occurrence of the durable path fragments.
func TestNoAdHocSecretPathConstruction(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repo root")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))

	// Each rule: a source fragment that only the named files may contain.
	// Test files are excluded wholesale (they build expected paths on purpose).
	rules := []struct {
		fragment string
		allowed  map[string]string // repo-relative path -> why
	}{
		{
			// The durable PSP secret-name prefix (#683), path.Join form.
			fragment: `path.Join("psps"`,
			allowed: map[string]string{
				"internal/merchants/secrets.go": "the canonical builder (PSPSecretName)",
			},
		},
		{
			// The same prefix, literal-path form (SEC-20: the hand-written
			// pattern that survived the psps rename and silently disarmed the
			// Solana plaintext-write guard was exactly this shape).
			fragment: `"psps/`,
			allowed:  map[string]string{},
		},
		{
			// The RETIRED prefix. Only the loud rename checks may name it.
			fragment: `"rail_merchant_accounts`,
			allowed: map[string]string{
				"internal/bootstrap/merchant_manifest.go": "legacy manifest-KEY rename check (config keys, not secret paths)",
				"embed/provision.go":                      "legacy manifest-KEY rename check (config keys, not secret paths)",
			},
		},
		{
			// The Vault merchant namespace (path.Join construction form).
			fragment: `"openrails", "merchants"`,
			allowed: map[string]string{
				"internal/merchants/secrets_vault.go": "the single vault namespace builder (pathFor/List)",
			},
		},
		{
			// The Vault merchant namespace (literal string form).
			fragment: `openrails/merchants/`,
			allowed: map[string]string{
				"internal/merchants/secrets_vault.go": "namespace shape documentation next to the builder",
			},
		},
	}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".codegraph":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue // doc comments may describe the shapes
			}
			for _, rule := range rules {
				if !strings.Contains(line, rule.fragment) {
					continue
				}
				if _, ok := rule.allowed[filepath.ToSlash(rel)]; ok {
					continue
				}
				t.Errorf("%s:%d: ad-hoc secret-path construction (%q) — derive paths via merchants.PSPSecretName / the vault store namespace builder, or extend the guard allowlist with a justification\n\t%s",
					rel, i+1, rule.fragment, trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
}
