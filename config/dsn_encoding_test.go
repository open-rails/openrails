package config

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetConnectionString_URLEncodesCredentials(t *testing.T) {
	t.Parallel()

	c := &DBConfig{
		Host:     "db.internal",
		Port:     "5432",
		Database: "billing",
		Username: "open rails",  // space is reserved
		Password: "p@ss/w:rd?#", // '@' and '/' would corrupt a raw DSN
	}

	dsn := c.GetConnectionString()

	// Must parse back to exactly the inputs — proving no host hijack via '@'.
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Equal(t, "postgresql", u.Scheme)
	require.Equal(t, "db.internal:5432", u.Host)
	require.Equal(t, "/billing", u.Path)
	require.Equal(t, "open rails", u.User.Username())
	pw, _ := u.User.Password()
	require.Equal(t, "p@ss/w:rd?#", pw)
	require.Equal(t, "require", u.Query().Get("sslmode"))

	// The raw '@' in the password must not appear unescaped before the host.
	require.NotContains(t, dsn[:len("postgresql://")+len("open%20rails")+40], "/w:rd?#@db.internal")
}

func TestGetConnectionString_URLPassthroughAndSSLMode(t *testing.T) {
	t.Parallel()

	require.Equal(t, "postgres://x/y", (&DBConfig{URL: "postgres://x/y"}).GetConnectionString())

	c := &DBConfig{Host: "h", Port: "1", Database: "d", Username: "u", Password: "p", SSLMode: "disable"}
	u, err := url.Parse(c.GetConnectionString())
	require.NoError(t, err)
	require.Equal(t, "disable", u.Query().Get("sslmode"))
}
