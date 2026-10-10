package money

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/railresolve"
)

var _ paymentmethods.CardHolders = (*MerchantCollectionAdapterBuilder)(nil)

// ReadHeldCard reads a stored card from whoever holds it (#1168): the NMI
// vault or Stripe payment method of its PSP, or its custodian's token.
// Hyperswitch's updater rewrites a method in place, so a read is how its
// changes are mirrored.
func (b *MerchantCollectionAdapterBuilder) ReadHeldCard(ctx context.Context, m gen.BillingPaymentMethod) (paymentmethods.HeldCard, error) {
	svc := b.merchants()
	if b == nil || b.DB == nil || svc == nil {
		return paymentmethods.HeldCard{}, errors.New("card holders are not armed")
	}
	mid := billing.MerchantID(m.MerchantID)
	ref := models.DerefStr(m.RailMethodRef)
	switch {
	case m.Custodian == models.CustodianPSP && m.Rail == string(models.RailNMI):
		client, ok, err := b.ResolveNMIClient(ctx, m.MerchantID, m.PspID)
		if err != nil || !ok {
			return paymentmethods.HeldCard{}, fmt.Errorf("read NMI vault: account unavailable: %v", err)
		}
		customer, found, err := client.GetCustomer(ctx, models.DerefStr(m.RailCustomerRef))
		if err != nil || !found {
			return paymentmethods.HeldCard{Gone: !found}, err
		}
		card, err := intents.NMIVaultCard(customer, ref)
		if err != nil {
			return paymentmethods.HeldCard{}, err
		}
		return paymentmethods.HeldCard{Card: paymentmethods.Card{Card: card}, Source: paymentmethods.SourceProviderRead}, nil
	case m.Custodian == models.CustodianPSP && m.Rail == string(models.RailStripe):
		service, ok, err := b.ResolveStripeEngineService(ctx, m.MerchantID, m.PspID)
		if err != nil || !ok {
			return paymentmethods.HeldCard{}, fmt.Errorf("read Stripe payment method: account unavailable: %v", err)
		}
		state, err := service.ReadPaymentMethodCard(ctx, ref)
		if err != nil || state == nil || state.Card == nil {
			return paymentmethods.HeldCard{Gone: err == nil}, err
		}
		return paymentmethods.HeldCard{Card: paymentmethods.Card{Card: *state.Card, Fingerprint: state.Fingerprint}, Source: paymentmethods.SourceProviderRead}, nil
	case m.Custodian == models.CustodianHyperSwitch && m.CustodianID != nil:
		custodian, ok, err := svc.CustodianScopeByID(ctx, mid, *m.CustodianID)
		if err != nil || !ok {
			return paymentmethods.HeldCard{}, fmt.Errorf("read HyperSwitch method: custodian unavailable: %v", err)
		}
		client, err := railresolve.HyperSwitchClient(b.Config, mid, custodian)
		if err != nil {
			return paymentmethods.HeldCard{}, err
		}
		method, err := client.GetMethod(ctx, ref, models.DerefStr(m.RailCustomerRef))
		if err != nil {
			return paymentmethods.HeldCard{}, err
		}
		card := models.ParseCard(method.Data.Card.Brand, method.Data.Card.Last4, method.MaskedExpiry())
		return paymentmethods.HeldCard{Card: paymentmethods.Card{Card: card}, Source: paymentmethods.SourceHyperswitchUpdater}, nil
	case m.Custodian == models.CustodianBasisTheory && m.CustodianID != nil:
		custodian, ok, err := svc.CustodianScopeByID(ctx, mid, *m.CustodianID)
		if err != nil || !ok {
			return paymentmethods.HeldCard{}, fmt.Errorf("read Basis Theory token: custodian unavailable: %v", err)
		}
		apiKey, err := b.requireCustodianSecret(ctx, svc, mid, custodian, custodians.SecretAPIKey)
		if err != nil {
			return paymentmethods.HeldCard{}, err
		}
		bt, err := basistheory.New(basistheory.Config{APIKey: apiKey, BaseURL: b.Endpoints.BTBaseURL, ReadOnly: b.Config != nil && config.IsProviderReadOnly(b.Config)})
		if err != nil {
			return paymentmethods.HeldCard{}, err
		}
		token, err := bt.GetToken(ctx, ref)
		if basistheory.IsNotFound(err) {
			return paymentmethods.HeldCard{Gone: true}, nil
		}
		if err != nil {
			return paymentmethods.HeldCard{}, err
		}
		card := models.Card{}
		if c := token.Card; c != nil {
			card = models.ParseCard(c.Brand, c.Last4, "")
			if c.ExpirationMonth >= 1 && c.ExpirationMonth <= 12 && c.ExpirationYear >= 2000 {
				card.ExpMonth, card.ExpYear = c.ExpirationMonth, c.ExpirationYear
			}
		}
		return paymentmethods.HeldCard{Card: paymentmethods.Card{Card: card, Fingerprint: token.Fingerprint}, Source: paymentmethods.SourceProviderRead}, nil
	}
	return paymentmethods.HeldCard{}, fmt.Errorf("no holder reads a %s card held by %s", m.Rail, m.Custodian)
}
