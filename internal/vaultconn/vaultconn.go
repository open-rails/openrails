// Package vaultconn binds the host's Vault client to the runtime's merchant
// configuration, platform card credentials and Solana Transit signing.
package vaultconn

import (
	"context"
	"errors"
	"log/slog"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/open-rails/openrails/internal/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/retry"
)

// ErrNoClient refuses a Vault mount named without a client to reach it.
var ErrNoClient = errors.New("Config names a Vault mount but Deps.Vault is nil")

// Connection is the runtime's Vault: the host's client over the mounts
// Config names, or none.
type Connection struct {
	// KV is the merchant configuration mount; nil when none is named.
	KV *vault.KVv2Adapter
	// PlatformKV holds platform card credentials independently of merchant configuration.
	PlatformKV *vault.KVv2Adapter
	// SolanaTransit signs for a vault_transit Solana cranker; nil without
	// Vault.
	SolanaTransit solanaint.TransitClient
	// Err is why the named mounts cannot be used: they refuse every
	// operation, and a KV mount keeps naming Vault as the merchant
	// configuration's source.
	Err error

	stop context.CancelFunc
}

// Open binds client to the mounts cfg names. A mount named without a client,
// or a malformed mount path, leaves Vault unusable, never an error: its
// operations refuse and Err reports why.
func Open(ctx context.Context, logger *slog.Logger, cfg *config.Config, client *vaultapi.Client) (*Connection, error) {
	mount := config.MerchantConfigKVMount(cfg)
	platformMount, err := config.CardVaultKVMount(cfg)
	if err != nil {
		return nil, err
	}
	if cfg == nil || client == nil && mount == "" && platformMount == "" && !config.SolanaTransitSigns(cfg) {
		return &Connection{}, nil
	}
	vc := cfg.Vault
	if vc == nil {
		vc = &config.VaultConfig{}
	}
	conn := &Connection{Err: config.ValidateVault(vc)}
	if client == nil {
		conn.Err = ErrNoClient
	}
	if conn.Err != nil {
		logger.ErrorContext(ctx, "vault: configuration refused; Vault-backed merchant configuration and signing are unavailable", logx.Err(conn.Err))
	} else if mount != "" {
		verify, stop := context.WithCancel(context.WithoutCancel(ctx))
		conn.stop = stop
		go verifyCapabilities(verify, logger, client, mount, vc.ScopePrefix)
	}
	transitMount := vc.TransitMount
	if transitMount == "" {
		transitMount = config.DefaultVaultTransitMount
	}
	conn.SolanaTransit = vault.NewTransitAdapter(client, transitMount).Refuse(conn.Err)
	if mount != "" {
		conn.KV = vault.NewKVv2Adapter(client, mount).Refuse(conn.Err)
	}
	if platformMount != "" {
		conn.PlatformKV = vault.NewKVv2Adapter(client, platformMount).Refuse(conn.Err)
	}
	return conn, nil
}

// Close stops the capability check. The host's client stays open.
func (c *Connection) Close() {
	if c != nil && c.stop != nil {
		c.stop()
	}
}

// State is why the named mounts cannot be used; nil when they can.
func (c *Connection) State() error {
	if c == nil {
		return nil
	}
	return c.Err
}

// verifyCapabilities reports a token that cannot serve the merchant
// configuration mount, retrying until Vault answers. Diagnostics only: reads
// and writes fail at runtime either way.
func verifyCapabilities(ctx context.Context, logger *slog.Logger, client *vaultapi.Client, kvMount, scopePrefix string) {
	_ = retry.Forever(ctx, func(ctx context.Context) error {
		caps, err := vault.SelfCapabilities(ctx, client, kvMount, scopePrefix)
		if err != nil {
			return err
		}
		if !caps.KVRead {
			logger.ErrorContext(ctx, "vault: the token cannot read the KV mount; merchant configuration is unavailable", "kv_mount", kvMount)
		} else if !caps.KVWrite {
			logger.WarnContext(ctx, "vault: read-only KV capability; merchant configuration edits fail")
		}
		return nil
	}, nil)
}
