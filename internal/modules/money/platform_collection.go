package money

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
)

// ResolveBasisTheoryNMICharge arms a saved vendor card on its accepted account.
// The caller owns the recurring agreement and durable submission fence.
func (b *MerchantCollectionAdapterBuilder) ResolveBasisTheoryNMICharge(ctx context.Context, method gen.BillingPaymentMethod, psp uuid.UUID) (*nmiproxy.Charger, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if mid.UUID() != method.MerchantID || !models.BasisTheoryCustody(method.Custodian) || method.CustodianID == nil {
		return nil, charge.ErrInstrumentChanged
	}
	if err := charge.ValidateEngineInstrument(method.Rail, charge.FreezeInstrument(method, psp), nil); err != nil {
		return nil, err
	}
	svc := b.merchants()
	if svc == nil || b.Config == nil || config.IsProviderReadOnly(b.Config) {
		return nil, errors.New("platform collection is unavailable")
	}
	scope, ok, err := b.resolveScope(ctx, svc, mid, method.Rail, &psp)
	if err != nil {
		return nil, err
	}
	if !ok || !scope.Declared || scope.Archived {
		return nil, errors.New("platform collection account is unavailable")
	}
	custody, err := svc.BasisTheoryCustodianConfig(ctx, mid, *method.CustodianID)
	if err != nil {
		return nil, err
	}
	if custody.Custodian != method.Custodian || b.BasisTheoryTransport != nil && custody.Environment != config.ClassTest {
		return nil, charge.ErrInstrumentChanged
	}
	base := custody.APIBaseURL
	if b.Endpoints.BTBaseURL != "" {
		base = b.Endpoints.BTBaseURL
	}
	bt, err := basistheory.New(basistheory.Config{APIKey: custody.APIKey, BaseURL: base, Transport: b.BasisTheoryTransport})
	if err != nil {
		return nil, err
	}
	token, err := bt.GetToken(ctx, *method.RailMethodRef)
	if err != nil {
		return nil, err
	}
	if token.ID != *method.RailMethodRef || token.Type != "card" || token.TenantID != custody.AccountID || custody.Container != "" && (len(token.Containers) != 1 || token.Containers[0] != custody.Container) {
		return nil, errors.New("platform card is outside the merchant's vault container")
	}
	posture, err := b.nmiProxyPosture(ctx, svc, mid, scope)
	if err != nil {
		return nil, err
	}
	return nmiproxy.New(bt, nmiproxy.GatewayConfig{SecurityKey: posture.SecurityKey, DirectPostURL: posture.DirectPostURL, Posture: posture}).WithSource(nmiproxy.Source{TokenID: token.ID}), nil
}

// platformRenewalAccount selects an account for a new attempt, not a retry of
// an accepted operation. Recurring reference eligibility belongs to the rail.
func platformRenewalAccount(ctx context.Context, configuration *merchants.Service, method gen.BillingPaymentMethod, agreement gen.BillingMandate) (uuid.UUID, error) {
	mid := billing.MerchantID(method.MerchantID)
	_, settings, err := configuration.MerchantSettings(ctx, mid)
	if err != nil {
		return uuid.Nil, err
	}
	accounts, err := configuration.ActivePSPScopesForRail(ctx, mid, method.Rail)
	if err != nil {
		return uuid.Nil, err
	}
	for _, account := range accounts {
		if !account.Declared || account.DuplicateAccount || len(settings.ChargePSPs) > 0 && !slices.Contains(settings.ChargePSPs, account.Key) {
			continue
		}
		switch account.Rail {
		case string(models.RailNMI):
			if agreement.Rail != account.Rail || nmiproxy.ValidateRecurringAccount(agreement.PspID, account.ID) != nil || account.Secret("security_key") == "" {
				continue
			}
			return account.ID, nil
		}
	}
	return uuid.Nil, fmt.Errorf("%w: no configured account can use the recurring reference from account %s", charge.ErrNoRoute, agreement.PspID)
}
