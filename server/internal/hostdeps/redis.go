package hostdeps

import (
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// RedisOptions are the redis section's client options.
func RedisOptions(c *hostconfig.RedisConfig) (*redis.Options, error) {
	if c == nil || strings.TrimSpace(c.URL) == "" {
		return nil, errors.New("redis: set redis.url (REDIS_URL)")
	}
	rawURL := strings.TrimSpace(c.URL)
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("redis.url: %w", redactURL(err, rawURL))
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
	if pem := strings.TrimSpace(c.CACert); pem != "" {
		if opts.TLSConfig == nil {
			return nil, errors.New("redis.ca_cert needs TLS: use a rediss:// URL")
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(pem)) {
			return nil, errors.New("redis.ca_cert holds no PEM certificate")
		}
		opts.TLSConfig.RootCAs = roots
	}
	return opts, nil
}

// redactURL keeps a URL's password out of a parse error.
func redactURL(err error, raw string) error {
	msg := err.Error()
	if i := strings.Index(raw, "://"); i >= 0 {
		if at := strings.LastIndex(raw, "@"); at > i {
			msg = strings.ReplaceAll(msg, raw[i+3:at], "…")
		}
	}
	return errors.New(msg)
}
