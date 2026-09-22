package embed

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/app"
)

// MerchantDeclaration configures the one merchant served by an embedded runtime.
// Construction reconciles Config idempotently. PSPs declares attribution-only
// identities; credentials and checkout availability remain owned by Config.
// Obtain the resulting merchant ID from the runtime's Client.
type MerchantDeclaration struct {
	Slug   string
	Config MerchantConfig
	PSPs   []PSPDeclaration
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
	id, err := upsertMerchantConfig(ctx, application, declaration.Slug, declaration.Config)
	if err != nil {
		return err
	}
	for _, psp := range declaration.PSPs {
		if _, err := declarePSP(ctx, application, id, psp); err != nil {
			return err
		}
	}
	return nil
}
