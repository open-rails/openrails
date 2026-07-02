package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// NMIClientForExistingSubscription resolves the NMI client that owns an already
// recorded subscription. New-work selectors must not be used for rows pinned to
// an archived provider account.
func NMIClientForExistingSubscription(ctx context.Context, database *db.DB, clients map[string]*nmi.NMIClient, sub *models.Subscription) (*nmi.NMIClient, string, bool, error) {
	if sub == nil {
		return nil, "", false, errors.New("subscription is nil")
	}
	key := strings.ToLower(string(sub.Rail))
	if sub.RailMerchantAccountID != nil {
		if database == nil {
			return nil, "", false, errors.New("provider account lookup unavailable")
		}
		account, err := database.Gen(ctx).GetRailMerchantAccount(ctx, *sub.RailMerchantAccountID)
		if err != nil {
			return nil, "", false, err
		}
		if !rails.SameRail(models.Rail(account.Rail), sub.Rail) {
			return nil, "", false, fmt.Errorf("provider account %s belongs to rail %s, not %s", account.ID, account.Rail, sub.Rail)
		}
		key = strings.TrimSpace(account.AccountID)
	}
	if key == "" || clients == nil {
		return nil, key, false, nil
	}
	client := clients[key]
	return client, key, client != nil, nil
}

func PaymentMethodMatchesSubscriptionProvider(pm *models.PaymentMethod, sub *models.Subscription) bool {
	if pm == nil || sub == nil || pm.RailMerchantAccountID == nil || sub.RailMerchantAccountID == nil {
		return true
	}
	return *pm.RailMerchantAccountID == *sub.RailMerchantAccountID
}
