package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Operator-mounted secrets: the default non-SaaS secret path. The operator
// renders secrets out of Vault (Vault Agent template, k8s secret volume, CSI)
// into a directory mounted read-only in the container — one file per secret,
// filename = the env-var name, content = the value. OpenRails needs no live
// Vault connection at runtime. Same directory convention as the doujins /
// hentai0 host apps; env always overrides files.

// DefaultSecretsPath is where operator-rendered secret files are expected;
// VAULT_SECRETS_PATH overrides it.
const DefaultSecretsPath = "/vault/secrets"

// SecretsPath returns the mounted secrets directory.
func SecretsPath() string {
	if p := strings.TrimSpace(os.Getenv("VAULT_SECRETS_PATH")); p != "" {
		return p
	}
	return DefaultSecretsPath
}

var secretFileNameShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// SecretFiles reads the mounted secrets dir into env-var-name -> value. An
// absent dir is nil (mounting is optional). Subdirectories, k8s "..data"
// symlink metadata, and files not named like env vars (e.g. the host app's own
// dash-cased secrets sharing the dir) are skipped; whitespace-trimmed empty
// values are skipped. An unreadable file is an error — a mounted secret that
// cannot be read must fail loudly, never silently drop.
func SecretFiles() (map[string]string, error) {
	dir := SecretsPath()
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
		raw, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- dir is operator-mounted (VAULT_SECRETS_PATH/default), name comes from os.ReadDir on that same dir, filtered to the env-var name shape above
		if err != nil {
			return nil, fmt.Errorf("read secret file %s: %w", name, err)
		}
		if v := strings.TrimSpace(string(raw)); v != "" {
			out[name] = v
		}
	}
	return out, nil
}
