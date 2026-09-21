package intents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/railresolve"
)

// NMIClientResolver arms the store-scoped NMI client for one intent merchant
// (the #725 credential plane; satisfied by money.MerchantCollectionAdapterBuilder).
// ok=false with nil err = no declared NMI account; err = declared but not
// armable (fail closed).
type NMIClientResolver interface {
	ResolveNMIClient(ctx context.Context, merchantID uuid.UUID, stampedAccountID *uuid.UUID) (*nmi.NMIClient, bool, error)
}

// resolveIntentNMIClient arms the intent merchant's NMI client from the armed
// rail state (#788 Layer C): the stamped provenance account when present,
// else the merchant's pull scope. ok=false = no declared NMI account; err =
// declared but not armable (fail closed — the caller parks, never charges).
func resolveIntentNMIClient(ctx context.Context, r NMIClientResolver, intent gen.OpenrailsRailIntent) (*nmi.NMIClient, bool, error) {
	if r == nil {
		return nil, false, errors.New("nmi client resolver is not configured")
	}
	return r.ResolveNMIClient(ctx, intent.MerchantID, intent.PspID)
}

// Sealed receipt constructors verify the returned client's identity themselves;
// a resolver's ok flag is not account provenance or proof of a non-nil client.
func resolveReceiptNMIClient(ctx context.Context, r NMIClientResolver, intent gen.OpenrailsRailIntent) (*nmi.NMIClient, error) {
	if intent.MerchantID == uuid.Nil || intent.PspID == nil || *intent.PspID == uuid.Nil {
		return nil, errors.New("receipt requires its accepted provider account")
	}
	client, ok, err := resolveIntentNMIClient(ctx, r, intent)
	if err != nil {
		return nil, err
	}
	if !ok || client == nil {
		return nil, errors.New("accepted provider account cannot be armed")
	}
	owner, account := client.AccountIdentity()
	if owner != intent.MerchantID || account != *intent.PspID {
		return nil, errors.New("NMI reader is armed for another provider account")
	}
	return client, nil
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
