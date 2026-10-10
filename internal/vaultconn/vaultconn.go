// Package vaultconn connects the runtime to HashiCorp Vault: the KV v2 mount
// merchant configuration lives in when one is named, and Transit for Solana
// signing. Name a KV mount and Vault holds merchant configuration; name only a
// Transit mount and it only signs.
package vaultconn

import (
	"context"
	"fmt"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/retry"
)

// DefaultTransitMount is used when config.VaultConfig.TransitMount is empty.
const DefaultTransitMount = "transit"

// AwaitTimeout bounds how long a one-off tool waits for Vault.
const AwaitTimeout = 30 * time.Second

// Connection is the runtime's Vault: none, a borrowed client, or one this
// process logs in to and supervises.
type Connection struct {
	closeAuth context.CancelFunc
	// KV is the merchant configuration mount; nil when none is named.
	KV *vault.KVv2Adapter
	// SolanaTransit signs for vault_transit Solana PSPs; nil without Vault.
	SolanaTransit solanaint.TransitClient
	// Auth supervises the owned login; nil without Vault or with a borrowed
	// client.
	Auth *vault.Supervisor
}

// Open connects to Vault when the configuration names it (cfg.Vault) or the
// host lends a client. Login runs in the background; a caller that needs
// Vault now awaits it. Without Vault nothing is opened.
func Open(ctx context.Context, cfg *config.Config, borrowed *vaultapi.Client) (*Connection, error) {
	if cfg == nil || (cfg.Vault == nil && borrowed == nil) {
		return &Connection{}, nil
	}
	vc := cfg.Vault
	if vc == nil {
		vc = &config.VaultConfig{}
	}
	conn := &Connection{}
	client := borrowed
	mount := config.MerchantConfigKVMount(cfg)
	if client == nil {
		authCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		var err error
		client, conn.Auth, err = vault.Login(authCtx, vaultConfig(vc))
		if err != nil {
			cancel()
			return nil, fmt.Errorf("vault login: %w", err)
		}
		conn.closeAuth = cancel
		if mount != "" {
			go verifyCapabilities(authCtx, client, conn.Auth, mount, vc.ScopePrefix)
		}
	}
	transitMount := vc.TransitMount
	if transitMount == "" {
		transitMount = DefaultTransitMount
	}
	conn.SolanaTransit = vault.NewTransitAdapter(client, transitMount).WithSupervisor(conn.Auth)
	if mount != "" {
		conn.KV = vault.NewKVv2Adapter(client, mount).WithReauthTrigger(conn.Auth)
	}
	return conn, nil
}

// Close stops only authentication supervision owned by this runtime. It never
// revokes a borrowed host token or closes a borrowed client.
func (c *Connection) Close() {
	if c != nil && c.closeAuth != nil {
		c.closeAuth()
	}
}

// State is the cached Vault auth state: nil when no owned Vault is in use or
// it is authenticated. It never touches the network.
func (c *Connection) State() error {
	if c == nil {
		return nil
	}
	return c.Auth.AuthState()
}

// Await waits up to timeout for the background Vault login.
func (c *Connection) Await(ctx context.Context, timeout time.Duration) error {
	if c == nil {
		return nil
	}
	if err := c.Auth.Wait(ctx, timeout); err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	return nil
}

// Probe is the live Vault check for a host dependency supervisor; nil when no
// owned Vault is in use.
func (c *Connection) Probe(ctx context.Context) error {
	if c == nil {
		return nil
	}
	return c.Auth.Probe(ctx)
}

func vaultConfig(vc *config.VaultConfig) vault.Config {
	return vault.Config{
		Address:    vc.Address,
		Namespace:  vc.Namespace,
		AuthMethod: vc.AuthMethod,
		Token:      vc.Token,
		RoleID:     vc.RoleID,
		SecretID:   vc.SecretID,
		K8sRole:    vc.K8sRole,
	}
}

// verifyCapabilities reports, once Vault authenticates, a token that cannot
// serve the merchant configuration mount. Diagnostics only: reads and writes
// fail at runtime either way.
func verifyCapabilities(ctx context.Context, client *vaultapi.Client, sup *vault.Supervisor, kvMount, scopePrefix string) {
	_ = retry.Forever(ctx, func(ctx context.Context) error {
		if err := sup.AuthState(); err != nil {
			return err
		}
		caps, err := vault.SelfCapabilities(ctx, client, kvMount, scopePrefix)
		if err != nil {
			return err
		}
		if !caps.KVRead {
			log.Errorf("vault: the token cannot read the KV mount %q; merchant configuration is unavailable", kvMount)
		} else if !caps.KVWrite {
			log.Warn("vault: read-only KV capability; merchant configuration edits fail")
		}
		return nil
	}, nil)
}
