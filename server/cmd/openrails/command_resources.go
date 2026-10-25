package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/email"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	"github.com/open-rails/openrails/server/internal/operator"
	"github.com/spf13/cobra"
)

// commandControlPlane owns only the account/directory resources an operator
// command needs. It never constructs an engine, HTTP surface or worker fleet.
type commandControlPlane struct {
	db        *db.DB
	cp        *controlplane.ControlPlane
	directory *merchants.Service
	cleanups  []func()
}

func (c *commandControlPlane) close() {
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		c.cleanups[i]()
	}
}

func openCLIControlPlane(ctx context.Context) (_ *commandControlPlane, err error) {
	h := hostconfig.FromContext(ctx)
	if h.Config == nil {
		return nil, fmt.Errorf("config not loaded")
	}
	if err := controlplane.RequireDefaultTenant(h.Auth); err != nil {
		return nil, err
	}
	c := &commandControlPlane{}
	defer func() {
		if err != nil {
			c.close()
		}
	}()
	c.db, err = openCLIDB(ctx, h.Config)
	if err != nil {
		return nil, err
	}
	c.cleanups = append(c.cleanups, func() { _ = c.db.Close() })
	c.directory, err = merchants.NewDirectoryService(c.db.DataPool())
	if err != nil {
		return nil, err
	}
	opts := controlplane.Options{LocalSignIn: h.LocalSignIn, Naming: h.Naming,
		Console: h.LocalSignIn && (h.AdminConsole == nil || !h.AdminConsole.Disabled)}
	akCfg, err := controlplane.AuthKit(h.Config, h.Auth, opts)
	if err != nil {
		return nil, err
	}
	// AuthKit requires its HTTP configuration while the merchant persona can
	// register OAuth clients. Retain that policy, but never mount its handler.
	key, err := h.SigningKey.Source()
	if err != nil {
		return nil, err
	}
	// Keep AuthKit's pool independent: its authority reads may run inside a
	// merchant transaction even when the configured billing pool has one slot.
	pool, err := pgxpool.NewWithConfig(ctx, c.db.Pool().Config())
	if err != nil {
		return nil, err
	}
	c.cleanups = append(c.cleanups, pool.Close)
	deps := authkit.Deps{Postgres: pool, KeySource: key}
	if h.SMTP != nil {
		sender, err := email.NewSMTP(*h.SMTP)
		if err != nil {
			return nil, err
		}
		deps.Email = controlplane.AuthKitSender{Sender: sender}
	}
	ak, err := authkit.New(ctx, akCfg, deps)
	if err != nil {
		return nil, err
	}
	c.cleanups = append(c.cleanups, func() { _ = ak.Close(context.WithoutCancel(ctx)) })
	c.cp, err = controlplane.New(ak, h.Config, akCfg, pool, opts)
	if err != nil {
		return nil, err
	}
	c.cp.BindMerchants(c.directory)
	return c, nil
}

func (c *commandControlPlane) loadMerchants(ctx context.Context) error {
	h := hostconfig.FromContext(ctx)
	opened, err := merchantbootstrap.OpenMerchants(ctx, h.Config, c.db)
	if err != nil {
		return err
	}
	overlap, err := config.WebhookSecretOverlapDuration(h.Config)
	if err != nil {
		opened.Close()
		return err
	}
	opened.Service.WithWebhookSecretOverlap(overlap)
	c.cleanups = append(c.cleanups, opened.Close)
	c.directory = opened.Service
	c.cp.BindMerchants(c.directory)
	return nil
}

func (c *commandControlPlane) ProvisionMerchant(ctx context.Context, req billing.ProvisionMerchantParams) (*billing.ProvisionMerchantResult, error) {
	return operator.ProvisionMerchant(ctx, c.cp, req)
}

func (c *commandControlPlane) GetMerchant(ctx context.Context, id billing.MerchantID) (*billing.Merchant, error) {
	return c.directory.DirectoryEntry(ctx, id)
}

func (c *commandControlPlane) RenameMerchant(ctx context.Context, id billing.MerchantID, req billing.RenameMerchantParams) (*billing.MerchantName, error) {
	m, err := c.cp.RenameMerchant(ctx, id, req.Name, req.ActorUserID, req.ActorUserID == "")
	if err != nil {
		return nil, err
	}
	return &billing.MerchantName{ID: m.ID, Name: m.Slug}, nil
}

func (c *commandControlPlane) DeleteMerchant(ctx context.Context, id billing.MerchantID) (*billing.Merchant, error) {
	return c.directory.SoftDelete(ctx, id)
}

func (c *commandControlPlane) RestoreMerchant(ctx context.Context, id billing.MerchantID) (*billing.Merchant, error) {
	return c.directory.Restore(ctx, id)
}

// Directory reads retain config-backed display names, but need no AuthKit.
func withCLIDirectory(cmd *cobra.Command, run func(context.Context, *db.DB, *merchants.Service) error) error {
	return withCLIDB(cmd, func(ctx context.Context, database *db.DB) error {
		cfg, _ := ctx.Value(config.ConfigContextKey).(*config.Config)
		opened, err := merchantbootstrap.OpenMerchants(ctx, cfg, database)
		if err != nil {
			return err
		}
		defer opened.Close()
		return run(ctx, database, opened.Service)
	})
}
