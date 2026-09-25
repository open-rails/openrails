package embed

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/merchant"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/retry"
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

// configureMerchant reports whether a Solana Transit signer still awaits Vault
// (see upsertMerchantConfig); confirmSigner completes it in the background.
func configureMerchant(ctx context.Context, application *app.App, declaration *MerchantDeclaration) (bool, error) {
	if declaration == nil {
		return false, nil
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
			return false, err
		}
		environment := config.ExpectedProviderEnvironment(application.Runtime.Config.IsTestMode())
		for _, psp := range declaration.PSPs {
			if err := merchants.AssertPSPUnowned(ctx, gen.New(application.Runtime.DB.DataPool()), selectedID, psp.Rail, environment, psp.AccountID); err != nil {
				return false, err
			}
		}
	}
	id, pending, err := upsertMerchantConfig(ctx, application, declaration.Slug, declaration.Config, true)
	if err != nil {
		return false, err
	}
	if declaration.MetadataApplication != nil {
		if _, err := service.ApplyMerchantMetadata(merchant.WithID(ctx, id), application.Runtime.DB, *declaration.MetadataApplication); err != nil {
			return false, err
		}
	}
	for _, psp := range declaration.PSPs {
		if _, err := hosttools.DeclarePSP(ctx, application, id, psp); err != nil {
			return false, err
		}
	}
	return pending, nil
}

// confirmSigner re-runs the declaration once Vault authenticates, arming a
// deferred Solana Transit PSP or re-deriving a stored identity from Vault,
// with capped full-jitter backoff until it succeeds.
func confirmSigner(application *app.App, declaration *MerchantDeclaration) {
	rt := application.Runtime
	rt.Go("solana transit signer", func(ctx context.Context) {
		err := retry.Forever(ctx, func(ctx context.Context) error {
			if err := rt.MerchantSecretBackend.State(); err != nil {
				return err
			}
			_, _, err := upsertMerchantConfig(ctx, application, declaration.Slug, declaration.Config, false)
			return err
		}, func(attempt int, err error) {
			if attempt == 0 {
				log.WithError(err).Warn("openrails embed: Solana Transit signer awaits Vault; retrying in the background")
			}
		})
		if err == nil {
			log.Info("openrails embed: Solana Transit signer confirmed by Vault")
		}
	})
}
