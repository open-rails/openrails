package embed

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/merchant"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/merchants"
)

// MerchantDeclaration configures the one merchant served by an embedded runtime.
// Construction reconciles Config idempotently. PSPs declares attribution-only
// identities; credentials and checkout availability remain owned by Config.
// Obtain the resulting merchant ID from the runtime's Client.
type MerchantDeclaration struct {
	Slug   string
	Config MerchantConfig
	PSPs   []PSPDeclaration
	// MetadataApplication applies an explicitly versioned metadata update after identity binding.
	MetadataApplication *openrails.MerchantConfigurationApplyParams
}

func validateMerchantDeclaration(declaration *MerchantDeclaration) error {
	if declaration == nil {
		return nil
	}
	if strings.TrimSpace(declaration.Slug) == "" {
		return fmt.Errorf("openrails embed: Merchant.Slug is required")
	}
	for i, psp := range declaration.PSPs {
		if strings.TrimSpace(psp.Key) == "" || strings.TrimSpace(psp.Rail) == "" || strings.TrimSpace(psp.AccountID) == "" {
			return fmt.Errorf("openrails embed: Merchant.PSPs[%d] requires key, rail and account ID", i)
		}
	}
	return nil
}

func configureMerchant(ctx context.Context, application *app.App, declaration *MerchantDeclaration) error {
	if declaration == nil {
		return nil
	}
	// Reject a provider identity owned by another merchant before provisioning
	// the new directory entry. The write boundary repeats this check for races.
	if len(declaration.PSPs) != 0 {
		var selectedID uuid.UUID
		selected, err := application.Runtime.Merchants.GetBySlug(ctx, declaration.Slug)
		switch {
		case err == nil:
			selectedID = selected.ID.UUID()
		case !errors.Is(err, merchants.ErrMerchantNotFound):
			return err
		}
		environment := config.ExpectedProviderEnvironment(application.Runtime.Config.IsTestMode())
		for _, psp := range declaration.PSPs {
			if err := merchants.AssertPSPUnowned(ctx, gen.New(application.Runtime.DB.DataPool()), selectedID, psp.Rail, environment, psp.AccountID); err != nil {
				return err
			}
		}
	}
	id, err := upsertMerchantConfig(ctx, application, declaration.Slug, declaration.Config)
	if err != nil {
		return err
	}
	if declaration.MetadataApplication != nil {
		if _, err := service.ApplyMerchantMetadata(merchant.WithID(ctx, id), application.Runtime.DB, *declaration.MetadataApplication); err != nil {
			return err
		}
	}
	for _, psp := range declaration.PSPs {
		if _, err := hosttools.DeclarePSP(ctx, application, id, psp); err != nil {
			return err
		}
	}
	return nil
}
