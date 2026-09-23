package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDatabaseUsesOnlyDatabaseConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "db:\n  url: postgres://file.invalid/db\n  schema: archive_test\nauth:\n  invalid_server_field: true\nprovider_write_mode: invalid\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_URL", "postgres://env.invalid/db")
	cfg, err := LoadDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DB.URL != "postgres://env.invalid/db" || cfg.DB.Schema != "archive_test" {
		t.Fatalf("database precedence or schema lost: %#v", cfg.DB)
	}
	if cfg.Auth != nil || cfg.Redis != nil {
		t.Fatal("returned a server configuration")
	}
	cfg, err = LoadDatabase(path, WithOverride("db.url", "postgres://flag.invalid/db"))
	if err != nil || cfg.DB.URL != "postgres://flag.invalid/db" {
		t.Fatalf("flag precedence lost: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("full server loader accepted invalid production config")
	}
}

func TestLoadDatabaseRefusesInvalidDatabaseConfiguration(t *testing.T) {
	for _, fields := range []string{"  unknown: true\n", "  schema: bad-schema\n", "  require_rls: false\n"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("db:\n"+fields), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDatabase(path); err == nil {
			t.Fatalf("accepted invalid database config %s", strings.TrimSpace(fields))
		}
	}
}
