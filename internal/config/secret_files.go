package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultSecretsPath is where a binary expects operator-rendered secret files
// unless its configuration names another directory.
const DefaultSecretsPath = "/vault/secrets"

var secretFileNameShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// SecretFiles reads a directory of operator-mounted secret files, one per
// environment-variable name, into name -> value. An absent directory is nil.
// Subdirectories, k8s "..data" symlink metadata, files not named like
// environment variables and empty values are skipped. An unreadable file is
// an error: a mounted secret that cannot be read must fail loudly.
func SecretFiles(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read secrets dir %s: %w", dir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, "..") || !secretFileNameShape.MatchString(name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- dir is the configured secrets directory; name comes from os.ReadDir on it, filtered to the env-var name shape above
		if err != nil {
			return nil, fmt.Errorf("read secret file %s: %w", name, err)
		}
		if v := strings.TrimSpace(string(raw)); v != "" {
			out[name] = v
		}
	}
	return out, nil
}
