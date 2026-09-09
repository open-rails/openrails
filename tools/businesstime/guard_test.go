package businesstime

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepositoryBusinessTimeGuard(t *testing.T) {
	root := os.Getenv("OPENRAILS_BUSINESS_TIME_ROOT")
	if root == "" {
		var err error
		root, err = filepath.Abs(filepath.Join("..", ".."))
		require.NoError(t, err)
	}
	var report bytes.Buffer
	err := Check(root, &report)
	require.NoError(t, err, report.String())
}

func TestGuardRejectsUnclassifiedAndStaleEntries(t *testing.T) {
	newFixture := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		for _, rel := range append([]string{"scripts"}, GuardPaths...) {
			require.NoError(t, os.MkdirAll(filepath.Join(root, rel), 0o755))
		}
		require.NoError(t, os.WriteFile(filepath.Join(root, "scripts", "business-time-allowlist.txt"), []byte("# test allowlist\n"), 0o644))
		return root
	}
	write := func(t *testing.T, root, rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
	check := func(root string) (string, error) {
		var report bytes.Buffer
		err := Check(root, &report)
		if err != nil {
			report.WriteString(err.Error())
		}
		return report.String(), err
	}

	t.Run("clean fixture", func(t *testing.T) {
		_, err := check(newFixture(t))
		require.NoError(t, err)
	})

	t.Run("unclassified clock", func(t *testing.T) {
		root := newFixture(t)
		write(t, root, "internal/modules/policy.go", "package fixture\nfunc expires() { expiry := time.Now().Add(24 * time.Hour) }\n")
		out, err := check(root)
		require.Error(t, err)
		require.Contains(t, out, "unclassified business-time usage")
	})

	t.Run("exact allowance and changed occurrence", func(t *testing.T) {
		root := newFixture(t)
		write(t, root, "internal/modules/policy.go", "package fixture\nfunc cache() {\nnow := time.Now()\n}\n")
		write(t, root, "scripts/business-time-allowlist.txt", "internal/modules/policy.go|now := time.Now()|infrastructure_time|test cache timing|1\n")
		_, err := check(root)
		require.NoError(t, err)

		path := filepath.Join(root, "internal/modules/policy.go")
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.WriteString("func businessPolicy() {\nnow := time.Now()\n}\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		out, err := check(root)
		require.Error(t, err)
		require.Contains(t, out, "occurrence changed")
	})

	t.Run("stale path", func(t *testing.T) {
		root := newFixture(t)
		write(t, root, "scripts/business-time-allowlist.txt", "internal/modules/missing.go|now := time.Now()|infrastructure_time|test missing path|1\n")
		out, err := check(root)
		require.Error(t, err)
		require.Contains(t, out, "stale clock allowlist path")
	})

	t.Run("invalid classification", func(t *testing.T) {
		root := newFixture(t)
		write(t, root, "internal/modules/policy.go", "package fixture\nfunc cache() { now := time.Now() }\n")
		write(t, root, "scripts/business-time-allowlist.txt", "internal/modules/policy.go|func cache() { now := time.Now() }|unreviewed|not a classification|1\n")
		out, err := check(root)
		require.Error(t, err)
		require.Contains(t, out, "invalid clock classification")
	})

	t.Run("missing scan directory", func(t *testing.T) {
		root := newFixture(t)
		require.NoError(t, os.Remove(filepath.Join(root, GuardPaths[1])))
		out, err := check(root)
		require.Error(t, err)
		require.Contains(t, out, "missing scan directory")
	})
}
