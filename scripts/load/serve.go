package main

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
)

// serveSpec is the standalone server a world runs: its own schema and
// ClickHouse database, its merchants provisioned with an API key each.
type serveSpec struct {
	DSN        string   `json:"dsn"`
	Schema     string   `json:"schema"`
	RedisURL   string   `json:"redis_url"`
	ClickHouse string   `json:"clickhouse"`
	CHDatabase string   `json:"ch_database"`
	Addr       string   `json:"addr"`
	PoolConns  int32    `json:"pool_conns"`
	Merchants  []string `json:"merchants"`
	Ready      string   `json:"ready"`
}

type serveReady struct {
	URL       string                   `json:"url"`
	Merchants map[string]readyMerchant `json:"merchants"`
}

type readyMerchant struct {
	ID  billing.MerchantID `json:"id"`
	Key string             `json:"key"`
}

// serveCmd runs the standalone server as run-server does (server.Run, with
// its workers) until it is signalled.
func serveCmd(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: load serve SPEC.json")
	}
	var spec serveSpec
	if err := readJSON(args[0], &spec); err != nil {
		return err
	}
	pool, err := openPool(ctx, spec.DSN, spec.PoolConns, nil)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	ch, err := openClickHouse(spec.ClickHouse)
	if err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	defer ch.Close()
	rdb, err := openRedis(spec.RedisURL)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	if rdb != nil {
		defer rdb.Close()
	}
	url := "http://" + spec.Addr
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := keys.SignerFromKey("load-server", key)
	if err != nil {
		return err
	}
	scopes := map[string][]string{billing.ScopeSelf: {"merchant:*"}}
	srv, err := server.New(ctx, server.Config{
		Engine: openrails.Config{Database: openrails.DatabaseConfig{Schema: spec.Schema, RiverSchema: spec.Schema}, ClickHouse: openrails.ClickHouseConfig{Database: spec.CHDatabase},
			PublicBillingBaseURL: url, ProviderWriteMode: openrails.ProviderWritesReadOnly, RateLimits: openrails.RateLimitsConfig{}},
		Auth: authkit.Config{
			DefaultTenantID: iam.NewTenantID(), Database: authkit.DatabaseConfig{Schema: spec.Schema + "_auth", RiverSchema: spec.Schema},
			Token: authkit.TokenConfig{Issuer: url, AllowPrivateNetworkJWKS: true}, Resource: authkit.ResourceConfig{ID: url, PublicURL: url, Scopes: scopes},
			AuthorizationServer: authkit.AuthorizationServerConfig{Resources: []authkit.ResourceServerConfig{{ID: url, Scopes: []string{billing.ScopeSelf}}},
				GroupClients: authkit.GroupClientsConfig{Scopes: []authkit.GroupClientScope{{Name: billing.ScopeSelf, Resource: url, Description: "Use own billing"}}}},
			Registration: authkit.RegistrationConfig{AllowMissingSenders: true}, TwoFactor: authkit.TwoFactorConfig{Mode: iam.TwoFactorDisabled},
			HTTP: &authkit.HTTPConfig{DirectPeerIP: true},
		},
		LocalSignIn: true, AdminConsole: &server.AdminConsole{Disabled: true},
		RouteGroups: openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true},
		Addr:        spec.Addr, ShutdownTimeout: 20 * time.Second,
	}, server.Deps{
		Engine: openrails.Deps{Postgres: pool, Redis: rdb, ClickHouse: ch, FXTransport: fxWire{}, Logger: quietLogger(os.Stderr)},
		Auth:   authkit.Deps{Postgres: pool, KeySource: keys.Static{Active: signer, Public: map[string]crypto.PublicKey{signer.KID(): signer.Public()}}},
	})
	if err != nil {
		return fmt.Errorf("server.New: %w", err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Run(ctx) }()
	if err := waitReady(ctx, url, served); err != nil {
		return err
	}
	ready := serveReady{URL: url, Merchants: map[string]readyMerchant{}}
	owner, ok := server.MerchantRole("owner")
	if !ok {
		return fmt.Errorf("no owner role")
	}
	for _, slug := range spec.Merchants {
		created, err := srv.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: slug})
		if err != nil {
			return fmt.Errorf("provision %s: %w", slug, err)
		}
		apiKey, err := srv.AuthKit().CreateAPIKey(ctx, iam.SystemIdentity(), iam.GroupByID(created.GroupID), iam.NewAPIKey{Name: "load backend", Role: owner})
		if err != nil {
			return fmt.Errorf("api key %s: %w", slug, err)
		}
		ready.Merchants[slug] = readyMerchant{ID: created.MerchantID, Key: apiKey.Secret}
	}
	if err := writeJSON(spec.Ready+".tmp", ready); err != nil {
		return err
	}
	if err := os.Rename(spec.Ready+".tmp", spec.Ready); err != nil {
		return err
	}
	return <-served
}

// waitReady waits for /health/ready, or after 45s for /health/live: an
// optional probe failing must not stop the run.
func waitReady(ctx context.Context, base string, served <-chan error) error {
	start := time.Now()
	for time.Since(start) < 90*time.Second {
		select {
		case err := <-served:
			return fmt.Errorf("server stopped before ready: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		url := base + "/health/ready"
		if time.Since(start) > 45*time.Second {
			url = base + "/health/live"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
	return fmt.Errorf("server not ready within 90s")
}
