package money

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func engineCustodianID(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}
func engineHyperSwitchPointer(custodian string, binding charge.HyperSwitchBinding) *charge.HyperSwitchBinding {
	if custodian == models.CustodianHyperSwitch {
		return &binding
	}
	return nil
}

// engineCollectionBinding checks psp, the account the charge goes through,
// against the card and freezes a custodian card's binding.
func engineCollectionBinding(ctx context.Context, q *gen.Queries, custody charge.Custody, method gen.BillingPaymentMethod, psp uuid.UUID, deployment string) (charge.HyperSwitchBinding, error) {
	var binding charge.HyperSwitchBinding
	if !charge.ChargeableOn(method, psp) {
		return binding, errors.New("engine account does not hold the card")
	}
	account, err := q.GetPSP(ctx, gen.GetPSPParams{MerchantID: method.MerchantID, ID: psp})
	if err != nil {
		return binding, err
	}
	if account.Rail != method.Rail {
		return binding, errors.New("engine account is mismatched")
	}
	if method.Custodian == models.CustodianHyperSwitch {
		return charge.FreezeHyperSwitchBinding(ctx, q, custody, method, psp, deployment)
	}
	if method.Custodian != models.CustodianPSP || method.CustodianID != nil {
		return binding, errors.New("engine card custody is unsupported")
	}
	return binding, nil
}

type recurringNMICharger interface {
	ChargeInitialRecurring(context.Context, charge.Request) (charge.Result, *nmi.CustomerVaultError, error)
	ChargeRecurringMIT(context.Context, charge.Request) (charge.Result, *nmi.CustomerVaultError, error)
}

func prepareEngineNMICharge(ctx context.Context, resolver CollectionPlane, method gen.BillingPaymentMethod, psp uuid.UUID, binding charge.HyperSwitchBinding) (recurringNMICharger, error) {
	if method.Custodian == models.CustodianHyperSwitch {
		return PrepareHyperSwitchCharge(ctx, resolver, method, psp, binding)
	}
	if method.Rail != "nmi" || method.Custodian != models.CustodianPSP || !charge.ChargeableOn(method, psp) {
		return nil, errors.New("engine NMI custody unsupported")
	}
	client, armed, err := resolver.ResolveNMIClient(ctx, method.MerchantID, &psp)
	if err != nil {
		return nil, err
	}
	if !armed || client == nil {
		return nil, errors.New("engine NMI account unavailable")
	}
	if err := client.PrepareRecurringSale(ctx, models.DerefStr(method.RailCustomerRef), models.DerefStr(method.RailMethodRef)); err != nil {
		return nil, err
	}
	return nmidirect.New(client), nil
}

func (b *MerchantCollectionAdapterBuilder) ResolveStripeEngineService(ctx context.Context, mid uuid.UUID, psp *uuid.UUID) (*subscriptions.StripeService, bool, error) {
	if b == nil || b.DB == nil || b.merchants() == nil || psp == nil {
		return nil, false, nil
	}
	scoped, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	if scoped.UUID() != mid {
		return nil, false, errors.New("engine account merchant mismatch")
	}
	scope, ok, err := b.resolveScope(ctx, b.merchants(), scoped, "stripe", psp)
	if err != nil || !ok {
		return nil, ok, err
	}
	service, err := b.stripeService(ctx, b.merchants(), scoped, scope)
	return service, err == nil, err
}
