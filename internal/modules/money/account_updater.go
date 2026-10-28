package money

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

var _ paymentmethods.AccountUpdaters = (*MerchantCollectionAdapterBuilder)(nil)

// AccountUpdater arms the real-time Account Updater for a card a Basis Theory
// vault holds (merchant-owned or platform), when its vault arms it and the card
// is Visa or Mastercard. Every other holder updates its cards in place.
func (b *MerchantCollectionAdapterBuilder) AccountUpdater(ctx context.Context, m gen.BillingPaymentMethod) (paymentmethods.AccountUpdater, bool, error) {
	ref := models.DerefStr(m.RailMethodRef)
	if !models.BasisTheoryCustody(m.Custodian) || m.CustodianID == nil || ref == "" {
		return nil, false, nil
	}
	if network := paymentmethods.CardNetwork(models.DerefStr(m.CardBrand)); network != "visa" && network != "mastercard" {
		return nil, false, nil
	}
	svc := b.merchants()
	if b == nil || svc == nil {
		return nil, false, errors.New("account updater: card vaults are not armed")
	}
	custody, err := svc.BasisTheoryCustodianConfig(ctx, billing.MerchantID(m.MerchantID), *m.CustodianID)
	if errors.Is(err, merchants.ErrCustodianNotDeclared) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !custody.AccountUpdater {
		return nil, false, nil
	}
	if custody.Custodian != m.Custodian || b.BasisTheoryTransport != nil && custody.Environment != config.ClassTest {
		return nil, false, errors.New("account updater: card custody identity changed")
	}
	base := custody.APIBaseURL
	if b.Endpoints.BTBaseURL != "" {
		base = b.Endpoints.BTBaseURL
	}
	client, err := basistheory.New(basistheory.Config{APIKey: custody.APIKey, BaseURL: base, ReadOnly: b.Config != nil && config.IsProviderReadOnly(b.Config), Transport: b.BasisTheoryTransport})
	if err != nil {
		return nil, false, err
	}
	return basisTheoryUpdater{client: client, token: ref, tenant: custody.AccountID, container: custody.Container}, true, nil
}

type basisTheoryUpdater struct {
	client                   *basistheory.Client
	token, tenant, container string
}

func (u basisTheoryUpdater) Update(ctx context.Context, key string) (paymentmethods.AccountUpdate, error) {
	res, err := u.client.UpdateCard(ctx, u.token, key)
	var apiErr *basistheory.APIError
	if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != http.StatusConflict && apiErr.Status != http.StatusTooManyRequests {
		return paymentmethods.AccountUpdate{}, fmt.Errorf("%w: %w", paymentmethods.ErrAccountUpdateRefused, err)
	}
	if err != nil {
		return paymentmethods.AccountUpdate{}, err
	}
	out := paymentmethods.AccountUpdate{ResultCode: strings.TrimSpace(res.ResultCode)}
	switch basistheory.ClassifyAccountUpdaterResult(out.ResultCode) {
	case basistheory.AUOutcomeUpdated:
		t := res.NewToken
		if t == nil || strings.TrimSpace(t.ID) == "" {
			return out, fmt.Errorf("%w: %s carries no token", paymentmethods.ErrAccountUpdateRefused, out.ResultCode)
		}
		// The new token must stay inside the card's accepted custody, as a
		// converted one must before it is stored.
		if t.TenantID != u.tenant || t.Type != "card" || u.container != "" && (len(t.Containers) != 1 || t.Containers[0] != u.container) {
			return out, fmt.Errorf("%w: updated card is outside its accepted custody", paymentmethods.ErrAccountUpdateRefused)
		}
		card := models.Card{}
		if c := t.Card; c != nil {
			card = models.ParseCard(c.Brand, c.Last4, "")
			if c.ExpirationMonth >= 1 && c.ExpirationMonth <= 12 && c.ExpirationYear >= 2000 {
				card.ExpMonth, card.ExpYear = c.ExpirationMonth, c.ExpirationYear
			}
		}
		out.Card = paymentmethods.Card{Card: card, Fingerprint: t.Fingerprint, RailMethodRef: t.ID}
	case basistheory.AUOutcomeClosed:
		out.Advice = paymentmethods.AdviceClosed
	case basistheory.AUOutcomeContactCardholder:
		out.Advice = paymentmethods.AdviceContactCardholder
	}
	return out, nil
}
