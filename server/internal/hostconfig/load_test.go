package hostconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolate points the loader at an empty secrets directory and a config path
// in a fresh directory, so only what the test sets is read.
func isolate(t *testing.T) (configPath, secrets string) {
	t.Helper()
	dir, secrets := t.TempDir(), t.TempDir()
	t.Setenv("VAULT_SECRETS_PATH", secrets)
	return filepath.Join(dir, "config.yaml"), secrets
}

// A retired setting refuses the load and names its replacement, whether a
// key, the environment or a mounted secret file sets it.
func TestRetiredSettingsRefuseTheLoad(t *testing.T) {
	for _, retired := range retiredSettings {
		want := retired.key + " / " + retired.env + " was removed: " + retired.now
		for source, set := range map[string]func(t *testing.T, configPath, secrets string){
			"key": func(t *testing.T, configPath, _ string) {
				if err := os.WriteFile(configPath, []byte(retired.key+": x\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			"env": func(t *testing.T, _, _ string) { t.Setenv(retired.env, "x") },
			"secret": func(t *testing.T, _, secrets string) {
				if err := os.WriteFile(filepath.Join(secrets, retired.env), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		} {
			t.Run(retired.key+"/"+source, func(t *testing.T) {
				configPath, secrets := isolate(t)
				set(t, configPath, secrets)
				for _, load := range []func(string, ...LoadOption) (*Config, error){Load, LoadDatabaseForCommand} {
					if _, err := load(configPath); err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("load: %v, want %q", err, want)
					}
				}
			})
		}
	}
}
