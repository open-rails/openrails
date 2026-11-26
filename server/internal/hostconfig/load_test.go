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
	t.Setenv("PUBLIC_URL", "http://localhost:3053")
	return filepath.Join(dir, "config.yaml"), secrets
}

// One database section holds the connection and the engine's schemas; a key
// neither reads refuses the load.
func TestOneDatabaseSection(t *testing.T) {
	write := func(t *testing.T, yaml string) string {
		configPath, _ := isolate(t)
		if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		return configPath
	}
	cfg, err := Load(write(t, "database:\n  url: postgres://u:p@db.internal:5432/x\n  schema: Shop\nclickhouse:\n  url: clickhouse://u:p@events.internal:9000/usage\nredis:\n  url: rediss://u:p@cache.internal:6380/2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Engine.ClickHouse.Database != "usage" || cfg.ClickHouse == nil || cfg.Redis == nil {
		t.Errorf("clickhouse %+v (%q), redis %+v", cfg.ClickHouse, cfg.Server.Engine.ClickHouse.Database, cfg.Redis)
	}
	if e := cfg.Server.Engine; cfg.Database.URL != "postgres://u:p@db.internal:5432/x" || e.Database.Schema != "shop" || e.Database.RiverSchema != "shop_river" {
		t.Fatalf("connection %+v, engine %+v", cfg.Database, e.Database)
	}
	const unknown = "unknown configuration keys refuse boot: "
	for yaml, want := range map[string]string{
		"database:\n  urll: x\n":                                 unknown + "database.urll",
		"redis:\n  adr: x\n":                                     unknown + "redis.adr",
		"auth:\n  tokn: {}\n":                                    unknown + "auth.tokn",
		"database:\n  sqltrace: true\n":                          unknown + "database.sqltrace",
		"clickhouse:\n  url: http://events.internal\n":           "clickhouse.url (CLICKHOUSE_URL) must be clickhouse://",
		"host: billing.internal\n":                               `invalid host "billing.internal"`,
		"db:\n  url: x\n":                                        unknown + "db",
		"auth:\n  keys:\n    allow_ephemeral_dev_keys: true\n":   unknown + "auth.keys.allow_ephemeral_dev_keys",
		"auth:\n  token:\n    issuer: https://billing.example\n": unknown + "auth.token",
		"local_sign_in: true\n":                                  unknown + "local_sign_in",
		"admin_console:\n  issuers: {}\n":                        unknown + "admin_console.issuers",
	} {
		if _, err := Load(write(t, yaml)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", yaml, err, want)
		}
	}
}

// Policy sections are set in config.yaml alone: a rate-limit bucket at 0
// there is unlimited, and a variable naming one refuses the load.
func TestPolicySectionsAreFileOnly(t *testing.T) {
	configPath, _ := isolate(t)
	if err := os.WriteFile(configPath, []byte("rate_limits:\n  payment:\n    requests_per_minute: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if limits := cfg.Server.Engine.RateLimits; limits.Payment.RequestsPerMinute != 0 || limits.Purchase.RequestsPerMinute != 10 {
		t.Fatalf("payment %+v, purchase %+v", limits.Payment, limits.Purchase)
	}
	for env, section := range map[string]string{
		"RATE_LIMITS_PAYMENT_REQUESTS_PER_MINUTE": "rate_limits",
	} {
		t.Run(env, func(t *testing.T) {
			configPath, _ := isolate(t)
			t.Setenv(env, "1")
			want := section + " is set in config.yaml, not the environment: unset " + env
			if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("load: %v, want %q", err, want)
			}
		})
	}
}

// The auth section holds auth.keys.path alone: any other key, from the
// file, a secret file or the environment, refuses boot.
func TestOnlyTheKeyPathIsAnAuthSetting(t *testing.T) {
	configPath, _ := isolate(t)
	t.Setenv("AUTH_KEYS_PATH", "/etc/openrails/auth")
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Keys.Path != "/etc/openrails/auth" {
		t.Fatalf("AUTH_KEYS_PATH reads auth.keys.path: %+v", cfg.Auth)
	}
	for env, key := range map[string]string{
		"AUTH_PASSWORD_MIN_LENGTH":              "auth.password_min_length",
		"AUTH_TOKEN_ALLOW_PRIVATE_NETWORK_JWKS": "auth.token_allow_private_network_jwks",
		"AUTH_ISSUERS":                          "auth.issuers",
	} {
		t.Run(env, func(t *testing.T) {
			configPath, _ := isolate(t)
			t.Setenv(env, "true")
			want := "unknown configuration keys refuse boot: " + key
			if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("load: %v, want %q", err, want)
			}
		})
	}
}

// The default ClickHouse is optional; a URL a source names, even the
// default's spelling, is the operator's, and an empty one declares none.
func TestOnlyTheDefaultClickHouseIsOptional(t *testing.T) {
	for name, c := range map[string]struct {
		yaml, env, secret string
		want              *ClickHouseConnection
	}{
		"nothing":      {want: &ClickHouseConnection{URL: defaultClickHouseURL, Default: true}},
		"empty file":   {yaml: "clickhouse: {}\n", want: &ClickHouseConnection{URL: defaultClickHouseURL, Default: true}},
		"file":         {yaml: "clickhouse:\n  url: clickhouse://events.internal:9000/usage\n", want: &ClickHouseConnection{URL: "clickhouse://events.internal:9000/usage"}},
		"env":          {env: defaultClickHouseURL, want: &ClickHouseConnection{URL: defaultClickHouseURL}},
		"secret file":  {secret: "clickhouse://u:p@events.internal:9000/usage", want: &ClickHouseConnection{URL: "clickhouse://u:p@events.internal:9000/usage"}},
		"empty url":    {yaml: "clickhouse:\n  url: \"\"\n"},
		"empty in env": {yaml: "clickhouse:\n  url: clickhouse://events.internal:9000/usage\n", env: " "},
	} {
		t.Run(name, func(t *testing.T) {
			configPath, secrets := isolate(t)
			if c.yaml != "" {
				if err := os.WriteFile(configPath, []byte(c.yaml), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if c.env != "" {
				t.Setenv("CLICKHOUSE_URL", c.env)
			}
			if c.secret != "" {
				if err := os.WriteFile(filepath.Join(secrets, "CLICKHOUSE_URL"), []byte(c.secret), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if (cfg.ClickHouse == nil) != (c.want == nil) || c.want != nil && *cfg.ClickHouse != *c.want {
				t.Fatalf("clickhouse %+v, want %+v", cfg.ClickHouse, c.want)
			}
		})
	}
}
