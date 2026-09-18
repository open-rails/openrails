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

// ErrPaymentMethodProviderAccountMismatch is returned when a durable
// payment-source update is asked to use an instrument owned by another PSP.
// Provider vault references are account-scoped; allowing this request to reach
// a source-account client could silently bill the wrong account or produce an
// unrepairable split. Cross-account cutover must use the explicit card
// re-entry workflow described by ProviderAccountCutoverPlan.
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

// NMIClientForExistingSubscription resolves the NMI client that owns an already
// recorded subscription. New-work selectors must not be used for rows pinned to
// an archived PSP.
// NMIClientSource arms the store-scoped NMI client for a subscription's
// merchant + stamped PSP (#788 Layer C; satisfied by
// money.MerchantCollectionAdapterBuilder). ok=false with nil err = the
// merchant declares no NMI account; err = declared but not armable (fail
// closed).
type NMIClientSource interface {
	ResolveNMIClient(ctx context.Context, merchantID uuid.UUID, stampedAccountID *uuid.UUID) (*nmi.NMIClient, bool, error)
}

// NMIClientForExistingSubscription resolves the NMI client that owns sub —
// the #704 stamped PSP when present, else the merchant's pull
// scope — from the armed psps state (#788).
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

// PaymentMethodMatchesSubscriptionProvider reports whether the instrument was
// vaulted by the same PSP that owns the subscription. or#893: both sides are
// always attributed, so this is a real comparison — it no longer waves through
// an unattributed row.
func PaymentMethodMatchesSubscriptionProvider(pm *models.PaymentMethod, sub *models.Subscription) bool {
	if pm == nil || sub == nil {
		return true
	}
	return pm.PspID == sub.PspID
}

// ValidatePaymentMethodProviderAccount enforces the provider-account boundary
// at the durable update seam. Callers may perform an earlier HTTP validation,
// but this check must remain at the side-effect boundary too.
func ValidatePaymentMethodProviderAccount(pm *models.PaymentMethod, sub *models.Subscription) error {
	if pm == nil || sub == nil {
		return errors.New("payment method and subscription are required")
	}
	if !PaymentMethodMatchesSubscriptionProvider(pm, sub) {
		return fmt.Errorf("%w: source=%s target=%s; card re-entry on the active provider is required", ErrPaymentMethodProviderAccountMismatch, sub.PspID, pm.PspID)
	}
	return nil
}
