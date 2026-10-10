package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrPaymentMethodProviderAccountMismatch refuses a payment-source update with
// an instrument owned by another PSP: vault references are account-scoped, so
// it could bill the wrong account. A cross-account move needs the card
// re-entered.
var ErrPaymentMethodProviderAccountMismatch = errors.New("payment method belongs to a different provider account")

// A custody remap retains the old PSP vault reference for correlation only.
// NMI's payment-source update cannot address a third-party custodian token.
var ErrPaymentMethodNotPSPVaulted = apperr.New(http.StatusConflict, "payment_method_not_psp_vaulted", "payment method is not held in the provider vault; collect the replacement card on this provider account")

func ValidatePaymentMethodSourceCustody(pm *models.PaymentMethod) error {
	if pm == nil || pm.Custodian != models.CustodianPSP || pm.CustodianID != nil {
		return ErrPaymentMethodNotPSPVaulted
	}
	return nil
}

// NMIClientSource arms the merchant's NMI client for a subscription's stamped
// PSP (money.MerchantCollectionAdapterBuilder). ok=false with nil err = no NMI
// account declared; err = declared but not armable (fail closed).
type NMIClientSource interface {
	ResolveNMIClient(ctx context.Context, merchantID uuid.UUID, stampedAccountID *uuid.UUID) (*nmi.NMIClient, bool, error)
}

// NMIClientForExistingSubscription resolves the NMI client that owns sub: its
// stamped PSP when present, else the merchant's pull scope. New-work selectors
// must not be used for rows pinned to an archived PSP.
func NMIClientForExistingSubscription(ctx context.Context, resolver NMIClientSource, sub *models.Subscription) (*nmi.NMIClient, string, bool, error) {
	if sub == nil {
		return nil, "", false, errors.New("subscription is nil")
	}
	if resolver == nil {
		return nil, "", false, errors.New("nmi client resolver is not configured")
	}
	client, ok, err := resolver.ResolveNMIClient(ctx, sub.MerchantID, &sub.PspID)
	if err != nil || !ok {
		return nil, strings.ToLower(string(sub.Rail)), false, err
	}
	return client, strings.ToLower(string(sub.Rail)), true, nil
}

// PaymentMethodMatchesSubscriptionProvider reports whether the subscription's
// PSP can charge the instrument: the PSP holding a PSP-held card, or any PSP
// for one a custodian holds.
func PaymentMethodMatchesSubscriptionProvider(pm *models.PaymentMethod, sub *models.Subscription) bool {
	if pm == nil || sub == nil {
		return true
	}
	return pm.ChargeableOn(sub.PspID)
}

// ValidatePaymentMethodProviderAccount enforces the provider-account boundary
// at the durable update seam. Callers may perform an earlier HTTP validation,
// but this check must remain at the side-effect boundary too.
func ValidatePaymentMethodProviderAccount(pm *models.PaymentMethod, sub *models.Subscription) error {
	if pm == nil || sub == nil {
		return errors.New("payment method and subscription are required")
	}
	if !PaymentMethodMatchesSubscriptionProvider(pm, sub) {
		return fmt.Errorf("%w: source=%s target=%s; card re-entry on the active provider is required", ErrPaymentMethodProviderAccountMismatch, sub.PspID, pm.HoldingPSP())
	}
	return nil
}
