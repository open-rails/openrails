package embedded

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// MerchantNameOptions selects the directory for a public-name boundary. A nil
// NameAuthority explicitly selects the AuthKit-free unbound host namespace.
type MerchantNameOptions struct {
	Config        *config.Config
	PGXPool       *pgxpool.Pool
	Name          string
	NameAuthority merchant.NameAuthority
}

// ResolveMerchantName captures immutable billing identity and the current public
// name once. Pass the UUID to import/maintenance helpers; never carry a mutable
// name into those operations as an alternate identity.
func ResolveMerchantName(ctx context.Context, opts MerchantNameOptions) (merchant.ID, string, error) {
	database, err := openEmbeddedDB(ctx, opts.Config, opts.PGXPool)
	if err != nil {
		return merchant.ID{}, "", err
	}
	if opts.PGXPool == nil {
		defer database.Close()
	}
	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return merchant.ID{}, "", err
	}
	directory.WithNameAuthority(opts.NameAuthority)
	selected, err := directory.GetBySlug(ctx, opts.Name)
	if err != nil {
		return merchant.ID{}, "", err
	}
	return selected.ID, selected.Slug, nil
}
