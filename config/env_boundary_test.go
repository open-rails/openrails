package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoLibraryEnvReads enforces the #712 doctrine: env is read exactly once,
// at the binary boundary, by the config-loading pipeline. Importable packages
// must NOT call os.Getenv / os.LookupEnv / os.Environ / syscall.Getenv behind
// the host application's back — in embedded mode the HOST owns the process env.
// Every value a library needs arrives as explicit config.
func TestNoLibraryEnvReads(t *testing.T) {
	needles := []string{"os.Getenv(", "os.LookupEnv(", "os.Environ(", "syscall.Getenv("}

	// Allowlisted path prefixes (relative to the module root). One-line
	// justification per entry — anything else that reads env FAILS.
	allowedPrefixes := map[string]string{
		"cmd/":                                   "binary boundary: the process entrypoint owns flags and env",
		"config/":                                "the config-loading pipeline — the ONE place env is read",
		"tests/":                                 "test binaries own their env (OPENRAILS_TEST_*, RAILS_* fixtures)",
		"scripts/":                               "operational tooling run as its own process, not importable library code",
		"internal/dbtest/":                       "test-support package: container/DSN discovery for test binaries",
		"internal/integrations/vault/vaulttest/": "test-support package: VAULT_ADDR/VAULT_TOKEN external-server override, mirrors dbtest",
		"internal/bootstrap/merchant_env.go":     "the BILLING_MERCHANTS_* overlay — part of the binary's config pipeline (#712 blesses it)",
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected module root at %s: %v", root, err)
	}

	var violations []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil // test files own their env
		}
		for prefix := range allowedPrefixes {
			if strings.HasPrefix(rel, prefix) {
				return nil
			}
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			for _, needle := range needles {
				if strings.Contains(line, needle) {
					violations = append(violations, fmt.Sprintf("%s:%d: %s", rel, lineNo, strings.TrimSpace(line)))
				}
			}
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(violations) > 0 {
		t.Errorf("library env reads found (#712: env is read once at the binary boundary; "+
			"move the knob into config and thread it as an explicit field):\n  %s",
			strings.Join(violations, "\n  "))
	}
}
