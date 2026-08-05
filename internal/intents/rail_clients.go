package intents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/railresolve"
)

// resolveIntentNMIClient arms the intent merchant's NMI client from the armed
// rail state (#788 Layer C): the stamped provenance account when present,
// else the merchant's pull scope. ok=false = no declared NMI account; err =
// declared but not armable (fail closed — the caller parks, never charges).
func resolveIntentNMIClient(ctx context.Context, r money.NMIClientResolver, intent gen.OpenrailsRailIntent) (*nmi.NMIClient, bool, error) {
	if r == nil {
		return nil, false, errors.New("nmi client resolver is not configured")
	}
	return r.ResolveNMIClient(ctx, intent.MerchantID, intent.PspID)
}

// ccbillDataLinkForMerchant arms the ctx merchant's CCBill DataLink client
// from the armed rail state (#788 Layer C). Fail closed: an unarmed rail or
// missing datalink credentials error (the caller parks/retries).
// endpointOverride is a test seam for fake DataLink servers.
func ccbillDataLinkForMerchant(ctx context.Context, cfg *config.Config, src railresolve.Source, endpointOverride string) (*ccbill.DataLinkClient, error) {
	if src == nil {
		return nil, errors.New("ccbill rail resolution is not configured")
	}
	proc, err := src.RailConfig(ctx, string(models.RailCCBill), "")
	if err != nil {
		return nil, err
	}
	c := proc.ToCCBillConfig()
	if strings.TrimSpace(c.DataLinkUsername) == "" || strings.TrimSpace(c.DataLinkPassword) == "" {
		return nil, fmt.Errorf("ccbill account %s has no datalink credentials", proc.EffectiveAccountID())
	}
	if cfg != nil {
		c.TestMode = cfg.IsTestMode()
	}
	dl := ccbill.NewDataLinkClient(c)
	if cfg != nil {
		dl.ReadOnly = cfg.IsProviderReadOnly()
	}
	if endpointOverride != "" {
		dl.BaseURL = endpointOverride
	}
	return dl, nil
}
