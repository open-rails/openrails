package payment

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	hscharge "github.com/open-rails/openrails/internal/modules/payments/rails/hyperswitch"
	"github.com/open-rails/openrails/internal/railresolve"
)

func (h *InitialMembershipIntentHandler) hyperSwitchCharger(ctx context.Context, _ gen.BillingProviderIntent, p InitialMembershipPayload, gateway *nmi.NMIClient) (*hscharge.Charger, error) {
	if p.HyperSwitch == nil {
		return nil, errors.New("initial membership custody is not armed")
	}
	return hyperSwitchProxy(ctx, h.Purchases.Config, h.database(), *p.HyperSwitch, p.Instrument, gateway)
}

// hyperSwitchProxy charges a HyperSwitch-held card into gateway. The custody
// read and contract check precede the durable fence: they prove the accepted
// binding and the held card, not a remote lock; Charge rechecks before
// dispatch.
func hyperSwitchProxy(ctx context.Context, cfg *config.Config, d *db.DB, accepted charge.HyperSwitchBinding, instrument charge.FrozenInstrument, gateway *nmi.NMIClient) (*hscharge.Charger, error) {
	if cfg == nil || cfg.HyperSwitch == nil || config.IsProviderReadOnly(cfg) || gateway == nil || gateway.ReadOnly || instrument.CustodianID == nil {
		return nil, errors.New("HyperSwitch custody is not armed")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	custodian, held, err := merchants.Of(d).CustodianScopeByID(ctx, mid, *instrument.CustodianID)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, charge.ErrInstrumentChanged
	}
	binding, err := charge.HyperSwitchBindingFromAccount(custodian, cfg.HyperSwitch.APIBaseURL)
	if err != nil || binding != accepted {
		return nil, charge.ErrInstrumentChanged
	}
	client, err := railresolve.HyperSwitchClient(cfg, mid, custodian)
	if err != nil {
		return nil, err
	}
	if err := client.CheckProxyContract(ctx, gateway.DirectPostURL); err != nil {
		return nil, err
	}
	method, err := client.GetMethod(ctx, instrument.RailMethodRef, instrument.RailCustomerRef)
	if err != nil {
		return nil, err
	}
	if method.ID != instrument.RailMethodRef {
		return nil, charge.ErrInstrumentChanged
	}
	return &hscharge.Charger{Client: client, Destination: gateway.DirectPostURL, SecurityKey: hyperswitch.Secret(gateway.SecurityKey), Posture: gateway}, nil
}
