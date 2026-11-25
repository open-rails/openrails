package hostconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
				if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("load: %v, want %q", err, want)
				}
			})
		}
	}
}

// One database section holds the connection and the engine's schemas; a key
// neither reads refuses the load, as does the former db section.
func TestOneDatabaseSection(t *testing.T) {
	write := func(t *testing.T, yaml string) string {
		configPath, _ := isolate(t)
		if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		return configPath
	}
	cfg, err := Load(write(t, "database:\n  url: postgres://u:p@db.internal:5432/x\n  schema: Shop\n  lease_conns: 2\nevent_retention: 48h\nroute_groups:\n  merchant_config: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Server.RouteGroups.MerchantConfig {
		t.Error("route_groups.merchant_config is read")
	}
	if e := cfg.Server.Engine; cfg.Database.URL != "postgres://u:p@db.internal:5432/x" || e.Database.Schema != "shop" || e.Database.RiverSchema != "shop_river" || e.Database.LeaseConns != 2 || e.EventRetention != 48*time.Hour {
		t.Fatalf("connection %+v, engine %+v, retention %s", cfg.Database, e.Database, e.EventRetention)
	}
	for yaml, want := range map[string]string{
		"database:\n  urll: x\n":        "unknown configuration keys refuse boot: database.urll",
		"redis:\n  adr: x\n":            "unknown configuration keys refuse boot: redis.adr",
		"auth:\n  tokn: {}\n":           "unknown configuration keys refuse boot: auth.tokn",
		"database:\n  sqltrace: true\n": "unknown configuration keys refuse boot: database.sqltrace",
		"db:\n  url: x\n":               "db was merged into database",
	} {
		if _, err := Load(write(t, yaml)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", yaml, err, want)
		}
	}
	configPath, _ := isolate(t)
	t.Setenv("DB_URL", "postgres://u:p@db.internal:5432/x")
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "db was merged into database") {
		t.Errorf("DB_URL: %v", err)
	}
}
