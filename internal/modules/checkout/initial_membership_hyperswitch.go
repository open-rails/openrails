package checkout

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	hscharge "github.com/open-rails/openrails/internal/modules/payments/rails/hyperswitch"
	"github.com/open-rails/openrails/internal/railresolve"
)

// The concrete custody read and contract check precede the durable fence. They
// prove the observed binding, not a remote lock; Charge rechecks before dispatch.
func (h *InitialMembershipIntentHandler) hyperSwitchCharger(ctx context.Context, in gen.BillingProviderIntent, p InitialMembershipPayload, gateway *nmi.NMIClient) (*hscharge.Charger, error) {
	if h.Checkout.Config == nil || h.Checkout.Config.HyperSwitch == nil || config.IsProviderReadOnly(h.Checkout.Config) || gateway.ReadOnly || p.HyperSwitch == nil {
		return nil, errors.New("initial membership custody is not armed")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	custodian, held, err := merchants.Of(h.database()).CustodianScopeByID(ctx, mid, *p.Instrument.CustodianID)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, charge.ErrInstrumentChanged
	}
	binding, err := charge.HyperSwitchBindingFromAccount(custodian, h.Checkout.Config.HyperSwitch.APIBaseURL)
	if err != nil || binding != *p.HyperSwitch {
		return nil, charge.ErrInstrumentChanged
	}
	client, err := railresolve.HyperSwitchClient(h.Checkout.Config, mid, custodian)
	if err != nil {
		return nil, err
	}
	if err := client.CheckProxyContract(ctx, gateway.DirectPostURL); err != nil {
		return nil, err
	}
	method, err := client.GetMethod(ctx, p.Instrument.RailMethodRef, p.Instrument.RailCustomerRef)
	if err != nil {
		return nil, err
	}
	if method.ID != p.Instrument.RailMethodRef {
		return nil, charge.ErrInstrumentChanged
	}
	return &hscharge.Charger{Client: client, Destination: gateway.DirectPostURL, SecurityKey: hyperswitch.Secret(gateway.SecurityKey), Posture: gateway}, nil
}
