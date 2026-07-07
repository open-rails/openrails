package subscriptions

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// NMIClientForExistingSubscription resolves the NMI client that owns an already
// recorded subscription. New-work selectors must not be used for rows pinned to
// an archived provider account.
// NMIClientSource arms the store-scoped NMI client for a subscription's
// merchant + stamped provider account (#788 Layer C; satisfied by
// money.MerchantCollectionAdapterBuilder). ok=false with nil err = the
// merchant declares no NMI account; err = declared but not armable (fail
// closed).
type NMIClientSource interface {
	ResolveNMIClient(ctx context.Context, merchantID uuid.UUID, stampedAccountID *uuid.UUID) (*nmi.NMIClient, bool, error)
}

// NMIClientForExistingSubscription resolves the NMI client that owns sub —
// the #704 stamped provider account when present, else the merchant's pull
// scope — from the armed rail_merchant_accounts state (#788).
func NMIClientForExistingSubscription(ctx context.Context, resolver NMIClientSource, sub *models.Subscription) (*nmi.NMIClient, string, bool, error) {
	if sub == nil {
		return nil, "", false, errors.New("subscription is nil")
	}
	if resolver == nil {
		return nil, "", false, errors.New("nmi client resolver is not configured")
	}
	client, ok, err := resolver.ResolveNMIClient(ctx, sub.MerchantID, sub.RailMerchantAccountID)
	if err != nil || !ok {
		return nil, strings.ToLower(string(sub.Rail)), false, err
	}
	return client, strings.ToLower(string(sub.Rail)), true, nil
}

func PaymentMethodMatchesSubscriptionProvider(pm *models.PaymentMethod, sub *models.Subscription) bool {
	if pm == nil || sub == nil || pm.RailMerchantAccountID == nil || sub.RailMerchantAccountID == nil {
		return true
	}
	return *pm.RailMerchantAccountID == *sub.RailMerchantAccountID
}
