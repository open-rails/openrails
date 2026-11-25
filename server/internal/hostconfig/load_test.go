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

// A retired setting refuses the load and names its replacement, whether a
// key, the environment or a mounted secret file sets it.
func TestRetiredSettingsRefuseTheLoad(t *testing.T) {
	for _, retired := range retiredSettings {
		want := retired.key + " / " + retired.env + " was removed: " + retired.now
		variable := strings.Replace(retired.env, "*", "NMI_GATEWAY_URL", 1)
		yaml := retired.key + ": x\n"
		if variable != retired.env {
			yaml = retired.key + ":\n  nmi_gateway_url: x\n"
		}
		for source, set := range map[string]func(t *testing.T, configPath, secrets string){
			"key": func(t *testing.T, configPath, _ string) {
				if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			"env": func(t *testing.T, _, _ string) { t.Setenv(variable, "x") },
			"secret": func(t *testing.T, _, secrets string) {
				if err := os.WriteFile(filepath.Join(secrets, variable), []byte("x"), 0o600); err != nil {
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
	for yaml, want := range map[string]string{
		"database:\n  urll: x\n":                       "unknown configuration keys refuse boot: database.urll",
		"redis:\n  adr: x\n":                           "unknown configuration keys refuse boot: redis.adr",
		"auth:\n  tokn: {}\n":                          "removed auth settings refuse boot: auth.tokn (AUTH_TOKN)",
		"database:\n  sqltrace: true\n":                "unknown configuration keys refuse boot: database.sqltrace",
		"clickhouse:\n  url: http://events.internal\n": "clickhouse.url (CLICKHOUSE_URL) must be clickhouse://",
		"host: 127.0.0.1\n":                            "host was removed",
		"db:\n  url: x\n":                              "db was merged into database",
		"auth:\n  default_tenant_id: 51336692-ef57-4cdb-a189-a8d388465c11\n": "auth.default_tenant_id (AUTH_DEFAULT_TENANT_ID)",
		"auth:\n  keys:\n    allow_ephemeral_dev_keys: true\n":               "auth.keys.allow_ephemeral_dev_keys (AUTH_KEYS_ALLOW_EPHEMERAL_DEV_KEYS)",
		"auth:\n  token:\n    issuer: https://billing.example\n":             "auth.token.issuer (AUTH_TOKEN_ISSUER): set public_url",
		"local_sign_in: true\n":           "local_sign_in / LOCAL_SIGN_IN was removed",
		"admin_console:\n  issuers: {}\n": "admin_console.issuers / ADMIN_CONSOLE_ISSUERS was removed",
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
	if cfg.Server.Auth.Keys.Path != "/etc/openrails/auth" {
		t.Fatalf("AUTH_KEYS_PATH reads auth.keys.path: %+v", cfg.Server.Auth)
	}
	for env, want := range map[string]string{
		"AUTH_PASSWORD_MIN_LENGTH":                "auth.password.min_length (AUTH_PASSWORD_MIN_LENGTH)",
		"AUTH_TOKEN_ALLOW_PRIVATE_NETWORK_JWKS":   "auth.token.allow_private_network_jwks (AUTH_TOKEN_ALLOW_PRIVATE_NETWORK_JWKS)",
		"AUTH_HTTP_DIRECT_PEER_IP":                "auth.http.direct_peer_ip (AUTH_HTTP_DIRECT_PEER_IP): set trusted_proxies",
		"AUTH_ISSUERS":                            "auth.issuers (AUTH_ISSUERS)",
		"AUTH_CONTROL_PLANE_ISSUER":               "auth.control_plane_issuer (AUTH_CONTROL_PLANE_ISSUER)",
		"AUTH_REGISTRATION_ALLOW_MISSING_SENDERS": "auth.registration.allow_missing_senders (AUTH_REGISTRATION_ALLOW_MISSING_SENDERS)",
	} {
		t.Run(env, func(t *testing.T) {
			configPath, _ := isolate(t)
			t.Setenv(env, "true")
			if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("load: %v, want %q", err, want)
			}
		})
	}
}
