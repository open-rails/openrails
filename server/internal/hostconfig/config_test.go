package hostconfig

import (
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDBConnectionStringVerifiesRemoteTLS(t *testing.T) {
	parts := DBConfig{Port: "5432", Database: "openrails", Username: "openrails", Password: "p@ss/word"}
	for _, c := range []struct {
		host, sslMode, rootCert, want string
	}{
		{host: "db.internal", want: "verify-full"},
		{host: "10.0.0.5", want: "verify-full"},
		{host: "localhost", want: "require"},
		{host: "127.0.0.1", want: "require"},
		{host: "::1", want: "require"},
		{host: "db.internal", sslMode: "disable", want: "disable"},
		{host: "db.internal", rootCert: "/etc/openrails/db-ca.pem", want: "verify-full"},
	} {
		db := parts
		db.Host, db.SSLMode, db.SSLRootCert = c.host, c.sslMode, c.rootCert
		u, err := url.Parse(db.ConnectionString())
		if err != nil {
			t.Fatalf("%s: %v", c.host, err)
		}
		if got := u.Query().Get("sslmode"); got != c.want {
			t.Errorf("%s with sslmode %q: sslmode %q, want %q", c.host, c.sslMode, got, c.want)
		}
		if got := u.Query().Get("sslrootcert"); got != c.rootCert {
			t.Errorf("%s: sslrootcert %q, want %q", c.host, got, c.rootCert)
		}
	}

	db := parts
	db.Host = "db.internal"
	parsed, err := pgconn.ParseConfig(db.ConnectionString())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.TLSConfig == nil || parsed.TLSConfig.InsecureSkipVerify || parsed.TLSConfig.ServerName != "db.internal" {
		t.Fatalf("a remote database's certificate and name must be verified: %+v", parsed.TLSConfig)
	}
	if parsed.Password != "p@ss/word" {
		t.Fatal("the password must survive the URL")
	}

	db.URL = "postgres://u:p@db.internal:5432/x?sslmode=disable"
	if db.ConnectionString() != db.URL {
		t.Fatal("db.url is used as given")
	}
}
