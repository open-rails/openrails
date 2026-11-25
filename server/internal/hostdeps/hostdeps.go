// Package hostdeps opens what the standalone server reaches outside its
// process, from its configuration: the Postgres pool, Redis, ClickHouse, the
// Vault session, the SMTP sender and AuthKit's key directory. The server
// borrows them; Close closes them.
package hostdeps

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/keys"
	"github.com/open-rails/helpers/deps"
	"github.com/open-rails/helpers/vault"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// Pool tuning for the pool the server opens.
const (
	maxConns        = 30
	maxConnLifetime = time.Hour
	maxConnIdleTime = 15 * time.Minute

	connectBaseDelay   = time.Second
	connectMaxDelay    = 5 * time.Second
	connectPingTimeout = 5 * time.Second

	// vaultWait bounds how long a one-off command waits for Vault.
	vaultWait = 30 * time.Second
	// defaultVaultAddress is Vault's own default.
	defaultVaultAddress = "https://127.0.0.1:8200"
	// defaultK8sJWTPath is the in-cluster service-account token.
	defaultK8sJWTPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// secretIDFile is the mounted secret file every approle login reads.
	secretIDFile = "VAULT_SECRET_ID"
)

// Opened is what Open opened.
type Opened struct {
	// Deps is the engine's: Postgres, and Redis, Vault and Email when
	// declared and well formed.
	Deps openrails.Deps
	// Degraded are the optional sections that are not served, with why.
	Degraded []config.Degradation
	// Keys is AuthKit's key source: auth.keys.path's directory; nil without
	// one.
	Keys keys.Source

	vault  *vault.Session
	closes []func()
}

// Open opens cfg's connections. It waits for the database, and a ClickHouse
// a source named, until ctx ends; a malformed optional section (redis, vault,
// email_smtp) is left out and reported in Degraded.
func Open(ctx context.Context, logger *slog.Logger, cfg *hostconfig.Config) (*Opened, error) {
	pool, err := Pool(ctx, logger, cfg, true)
	if err != nil {
		return nil, err
	}
	o := &Opened{Deps: openrails.Deps{Postgres: pool}}
	o.closes = append(o.closes, pool.Close)
	if cfg.Redis != nil {
		if opts, err := RedisOptions(cfg.Redis); err != nil {
			o.degrade("redis", fmt.Errorf("%w; abuse state is kept in this process's memory", err))
		} else {
			logger.InfoContext(ctx, "Redis declared", "addr", opts.Addr, "tls", opts.TLSConfig != nil, "acl_user", opts.Username != "")
			client := redis.NewClient(opts)
			o.Deps.Redis = client
			o.closes = append(o.closes, func() { _ = client.Close() })
		}
	}
	if cfg.ClickHouse != nil {
		conn, err := ClickHouse(ctx, logger, cfg.ClickHouse)
		if err != nil {
			o.Close()
			return nil, err
		}
		if conn != nil {
			o.closes = append(o.closes, func() { _ = conn.Close() })
			// The event store is waited for like the database: usage lives there.
			if err := pingUntil(ctx, logger, "clickhouse", conn.Ping); err != nil {
				o.Close()
				return nil, fmt.Errorf("clickhouse: %w", err)
			}
			logger.InfoContext(ctx, "ClickHouse declared", "database", cfg.Server.Engine.ClickHouse.Database)
			o.Deps.ClickHouse = conn
		}
	}
	if cfg.Vault != nil {
		if session, err := Vault(context.WithoutCancel(ctx), cfg.Vault); err != nil {
			o.degrade("vault.login", fmt.Errorf("%w; Vault-backed merchant configuration and signing are unavailable", err))
		} else {
			o.vault, o.Deps.Vault = session, session.Client()
			o.closes = append(o.closes, session.Close)
		}
	}
	if cfg.SMTP != nil {
		if sender, err := NewEmailSender(*cfg.SMTP); err != nil {
			o.degrade("smtp", fmt.Errorf("%w; OpenRails sends no email", err))
		} else {
			o.Deps.Email = sender
		}
	}
	source, closeKeys, err := AuthKeys(logger, cfg)
	if err != nil {
		o.Close()
		return nil, err
	}
	o.Keys = source
	o.closes = append(o.closes, closeKeys)
	return o, nil
}

// AuthKeys watches auth.keys.path's keys.json and totp.key. Without the
// setting it is nil: AuthKit reads /vault/auth, or generates development
// keys for a loopback issuer.
func AuthKeys(logger *slog.Logger, cfg *hostconfig.Config) (keys.Source, func(), error) {
	path := strings.TrimSpace(cfg.Auth.Keys.Path)
	if path == "" {
		return nil, func() {}, nil
	}
	source, err := keys.Watch(path, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("auth.keys.path: %w", err)
	}
	return source, source.Close, nil
}

func (o *Opened) degrade(field string, err error) {
	o.Degraded = append(o.Degraded, config.Degradation{Field: field, Err: err})
}

// Report reports to rt's readiness what Open left out and the Vault
// session's health, which a supervisor probes until Close.
func (o *Opened) Report(ctx context.Context, rt *app.Runtime) {
	rt.RefuseDegraded(o.Degraded)
	if o.vault == nil {
		return
	}
	ctx, stop := context.WithCancel(ctx)
	o.closes = append(o.closes, stop)
	sup := deps.New()
	dep := sup.Add("vault_login", deps.Optional, o.vault.Probe, nil)
	sup.Start(ctx)
	rt.ReportDependency("vault_login", func() error {
		if st := dep.Status(); !st.Up {
			return errors.New(st.LastError)
		}
		return nil
	})
}

// Close closes what Open opened, the pool last.
func (o *Opened) Close() {
	for i := len(o.closes) - 1; i >= 0; i-- {
		o.closes[i]()
	}
}

// Pool opens the database section's pool. retry waits for the database until ctx
// ends, as a serving process does; otherwise it tries once.
func Pool(ctx context.Context, logger *slog.Logger, cfg *hostconfig.Config, retry bool) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.Database.DSN(os.Getenv))
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	pc.MaxConns, pc.MaxConnLifetime, pc.MaxConnIdleTime = maxConns, maxConnLifetime, maxConnIdleTime
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}
	ping := pool.Ping
	if retry {
		ping = func(ctx context.Context) error { return pingUntil(ctx, logger, "database", pool.Ping) }
	}
	if err := ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// pingUntil pings until what (database, clickhouse) answers or ctx ends: one
// that is failing over is waited for, never a boot failure.
func pingUntil(ctx context.Context, logger *slog.Logger, what string, ping func(context.Context) error) error {
	started, delay := time.Now(), connectBaseDelay
	for {
		pingCtx, cancel := context.WithTimeout(ctx, connectPingTimeout)
		err := ping(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		waited := time.Since(started).Round(time.Second)
		logger.WarnContext(ctx, what+" ping failed; retrying", logx.Err(err), "waiting_for", waited.String(), "retry_in", delay.String())
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("failed to connect to %s after %s (last: %v): %w", what, waited, err, ctx.Err())
		case <-timer.C:
		}
		delay = min(2*delay, connectMaxDelay)
	}
}

// Vault starts the vault section's session (helpers/vault): it logs in in
// the background. Only a malformed login or an unreadable CA file fails it.
func Vault(ctx context.Context, c *hostconfig.VaultLogin) (*vault.Session, error) {
	var caCert []byte
	if path := strings.TrimSpace(c.CACert); path != "" {
		var err error
		if caCert, err = os.ReadFile(path); err != nil { // #nosec G304 -- operator-configured CA path
			return nil, fmt.Errorf("vault.ca_cert: %w", err)
		}
	}
	staticSecretID := strings.TrimSpace(c.SecretID)
	return vault.Login(ctx, vault.Config{
		Address:        cmp.Or(strings.TrimSpace(c.Address), defaultVaultAddress),
		Namespace:      c.Namespace,
		CACert:         caCert,
		Method:         vault.Method(strings.TrimSpace(c.AuthMethod)),
		Token:          c.Token,
		RoleID:         c.RoleID,
		KubernetesRole: c.K8sRole,
		Mount:          c.AuthMount,
		SecretID: func(context.Context) (string, error) {
			if c.SecretsPath == "" {
				return staticSecretID, nil
			}
			files, err := config.SecretFiles(c.SecretsPath)
			if err != nil {
				return "", err
			}
			if v, ok := files[secretIDFile]; ok {
				return v, nil
			}
			return staticSecretID, nil
		},
		KubernetesJWT: func(context.Context) (string, error) {
			// kubelet rotates the file in place: every login reads it.
			raw, err := os.ReadFile(defaultK8sJWTPath)
			return string(raw), err
		},
	})
}

// AwaitVault waits, up to 30s, for a one-off command's session to
// authenticate.
func AwaitVault(ctx context.Context, session *vault.Session) error {
	ctx, cancel := context.WithTimeout(ctx, vaultWait)
	defer cancel()
	for {
		err := session.Probe(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("vault: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
