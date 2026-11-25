package hostconfig

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Without database.url, libpq's variables name the database, and a database
// off this machine has its certificate and name verified unless PGSSLMODE
// says otherwise; with neither, the local Compose database.
func TestTheDatabaseFollowsLibpqAndVerifiesRemoteTLS(t *testing.T) {
	for name, c := range map[string]struct {
		env  map[string]string
		url  string
		want string
	}{
		"database.url":            {url: "postgres://u:p@db.internal:5432/x?sslmode=disable", want: "postgres://u:p@db.internal:5432/x?sslmode=disable"},
		"nothing":                 {want: defaultDatabaseURL},
		"a remote PGHOST":         {env: map[string]string{"PGHOST": "db.internal", "PGUSER": "openrails"}, want: "sslmode=verify-full"},
		"a loopback PGHOST":       {env: map[string]string{"PGHOST": "127.0.0.1"}, want: ""},
		"a socket PGHOST":         {env: map[string]string{"PGHOST": "/var/run/postgresql"}, want: ""},
		"PGSSLMODE named":         {env: map[string]string{"PGHOST": "db.internal", "PGSSLMODE": "require"}, want: ""},
		"PGDATABASE on localhost": {env: map[string]string{"PGDATABASE": "openrails"}, want: ""},
	} {
		getenv := func(k string) string { return c.env[k] }
		if got := (DatabaseConnection{URL: c.url}).DSN(getenv); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	t.Setenv("PGHOST", "db.internal")
	t.Setenv("PGPASSWORD", "p@ss/word")
	parsed, err := pgconn.ParseConfig((DatabaseConnection{}).DSN(func(k string) string {
		if strings.HasPrefix(k, "PG") && k != "PGSSLMODE" {
			return map[string]string{"PGHOST": "db.internal"}[k]
		}
		return ""
	}))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.TLSConfig == nil || parsed.TLSConfig.InsecureSkipVerify || parsed.TLSConfig.ServerName != "db.internal" || parsed.Password != "p@ss/word" {
		t.Fatalf("a remote database's certificate and name must be verified, its PGPASSWORD used: %+v", parsed)
	}
}
