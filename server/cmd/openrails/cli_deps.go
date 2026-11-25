package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	"github.com/open-rails/openrails/server/internal/hostdeps"
)

// cli holds the connections a one-off command opens on first use; main
// closes them as the process exits.
var cli struct {
	mu      sync.Mutex
	pool    *pgxpool.Pool
	vault   *vaultapi.Client
	vaulted bool
	closes  []func()
}

func closeCLI() {
	cli.mu.Lock()
	defer cli.mu.Unlock()
	for i := len(cli.closes) - 1; i >= 0; i-- {
		cli.closes[i]()
	}
	cli.closes = nil
}

// cliPool is the command's pool, opened once with one connection attempt.
func cliPool(ctx context.Context) (*pgxpool.Pool, error) {
	cli.mu.Lock()
	defer cli.mu.Unlock()
	if cli.pool != nil {
		return cli.pool, nil
	}
	h := hostconfig.FromContext(ctx)
	if h == nil {
		return nil, fmt.Errorf("config not loaded")
	}
	connect, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := hostdeps.Pool(connect, slog.Default(), h, false)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	cli.pool = pool
	cli.closes = append(cli.closes, pool.Close)
	return pool, nil
}

// cliVault is the command's Vault client, logged in once and awaited; nil
// without a vault section.
func cliVault(ctx context.Context) (*vaultapi.Client, error) {
	cli.mu.Lock()
	defer cli.mu.Unlock()
	if cli.vaulted {
		return cli.vault, nil
	}
	h := hostconfig.FromContext(ctx)
	if h == nil || h.Vault == nil {
		cli.vaulted = true
		return nil, nil
	}
	session, err := hostdeps.Vault(context.WithoutCancel(ctx), h.Vault)
	if err != nil {
		return nil, err
	}
	if err := hostdeps.AwaitVault(ctx, session); err != nil {
		session.Close()
		return nil, err
	}
	cli.vault, cli.vaulted = session.Client(), true
	cli.closes = append(cli.closes, session.Close)
	return cli.vault, nil
}

// cliEventStore is the deployment's event store, checked and not migrated;
// nil without a clickhouse section or with an unreachable default one.
func cliEventStore(ctx context.Context) (*eventstore.Store, error) {
	h := hostconfig.FromContext(ctx)
	if h == nil || h.ClickHouse == nil {
		return nil, nil
	}
	conn, err := hostdeps.ClickHouse(ctx, slog.Default(), h.ClickHouse)
	if err != nil || conn == nil {
		return nil, err
	}
	cli.mu.Lock()
	cli.closes = append(cli.closes, func() { _ = conn.Close() })
	cli.mu.Unlock()
	return eventstore.Open(ctx, conn, config.ClickHouseDatabase(&h.Server.Engine), eventstore.Options{Validate: true, Logger: slog.Default()})
}

// cliDeps is the command's pool and Vault client.
func cliDeps(ctx context.Context) (*pgxpool.Pool, *vaultapi.Client, error) {
	pool, err := cliPool(ctx)
	if err != nil {
		return nil, nil, err
	}
	vault, err := cliVault(ctx)
	if err != nil {
		return nil, nil, err
	}
	return pool, vault, nil
}

// openCLIDB is the command's database over its own pool, which Close closes.
// Tenant operations carry their authorized merchant explicitly; PostgreSQL
// role flags do not select a tenant.
func openCLIDB(ctx context.Context) (*db.DB, error) {
	h := hostconfig.FromContext(ctx)
	if h == nil {
		return nil, fmt.Errorf("config not loaded")
	}
	connect, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := hostdeps.Pool(connect, slog.Default(), h, false)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	database, err := db.NewOwningPGXPool(slog.Default(), pool, config.SchemaName(&h.Server.Engine))
	if err != nil {
		pool.Close()
		return nil, err
	}
	return database, nil
}
