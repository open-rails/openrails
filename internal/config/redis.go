package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisConfig is a Redis connection: Addr, or a redis:// or rediss:// URL.
type RedisConfig struct {
	// Addr is host:port.
	Addr string
	// URL is redis://[user[:password]@]host[:port][/db], or rediss:// over
	// TLS, in place of Addr, DB and TLS. Username and Password, when set, are
	// used over the URL's.
	URL string
	// Username is the ACL user; empty is the default user.
	Username string
	Password string
	DB       int
	// TLS connects over TLS, verifying the server against the system roots,
	// or against CACert, which implies TLS.
	TLS bool
	// CACert is the PEM bundle of the CA that signed the server's certificate.
	CACert string
}

// RedisOptions are c's client options.
func RedisOptions(c *RedisConfig) (*redis.Options, error) {
	if c == nil {
		return nil, errors.New("redis: no configuration")
	}
	addr, rawURL := strings.TrimSpace(c.Addr), strings.TrimSpace(c.URL)
	var opts *redis.Options
	switch {
	case addr != "" && rawURL != "":
		return nil, errors.New("redis.addr and redis.url are exclusive")
	case rawURL != "":
		if c.DB != 0 || c.TLS {
			return nil, errors.New("redis.url names the database and TLS (rediss://): drop redis.db and redis.tls")
		}
		var err error
		if opts, err = redis.ParseURL(rawURL); err != nil {
			return nil, fmt.Errorf("redis.url: %w", redactRedisURL(err, rawURL))
		}
		if strings.TrimSpace(c.CACert) != "" && opts.TLSConfig == nil {
			return nil, errors.New("redis.ca_cert needs TLS: use a rediss:// URL")
		}
	case addr != "":
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return nil, fmt.Errorf("redis.addr %q: %w", addr, err)
		}
		opts = &redis.Options{Addr: addr, DB: c.DB}
		if c.TLS || strings.TrimSpace(c.CACert) != "" {
			host, _, _ := net.SplitHostPort(addr)
			opts.TLSConfig = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		}
	default:
		return nil, errors.New("redis: set redis.addr (REDIS_ADDR) or redis.url (REDIS_URL)")
	}
	// A request that counts in Redis waits on it: a Redis that does not
	// answer fails it fast, and the caller counts elsewhere.
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 2 * time.Second
	}
	if opts.DialerRetries == 0 {
		opts.DialerRetries = 1
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = 1
	}
	if c.Username != "" {
		opts.Username = c.Username
	}
	if c.Password != "" {
		opts.Password = c.Password
	}
	if pem := strings.TrimSpace(c.CACert); pem != "" {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(pem)) {
			return nil, errors.New("redis.ca_cert holds no PEM certificate")
		}
		opts.TLSConfig.RootCAs = roots
	}
	return opts, nil
}

// redactRedisURL keeps a URL's password out of a parse error.
func redactRedisURL(err error, raw string) error {
	msg := err.Error()
	if i := strings.Index(raw, "://"); i >= 0 {
		if at := strings.LastIndex(raw, "@"); at > i {
			msg = strings.ReplaceAll(msg, raw[i+3:at], "…")
		}
	}
	return errors.New(msg)
}
