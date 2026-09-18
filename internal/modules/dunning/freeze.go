package dunning

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ErrNothingToFreeze refuses a rebill whose instrument or price cannot be
// frozen before submission; nothing is enqueued.
var ErrNothingToFreeze = errors.New("rebill cannot be frozen")

// FreezeRebill is the period's rebill charge frozen before submission
// (#809 R4): the subscription's current instrument and its customer vault,
// and the price amount and currency. The worker and retry-now enqueue exactly
// this provider-vault recurring operation; the verifier and operator resolution accept only a provider sale that
// matches it, and a confirmed charge records exactly this amount.
func FreezeRebill(ctx context.Context, database *db.DB, sub *models.Subscription) (intents.ManualRebillPayload, error) {
	var p intents.ManualRebillPayload
	if sub == nil || sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return p, fmt.Errorf("%w: no current period", ErrNothingToFreeze)
	}
	// recurring=rebill_subscription always charges an NMI vault and billing
	// record. Local third-party custody never exempts its receipt from that
	// vault identity; a card-data proxy sale needs its own qualified operation.
	pm := sub.PaymentMethod
	if pm == nil || strings.TrimSpace(pm.RailMethodRef) == "" || strings.TrimSpace(sub.RailSubscriptionID) == "" || strings.TrimSpace(pm.RailCustomerRef) == "" {
		return p, fmt.Errorf("%w: no chargeable payment method", ErrNothingToFreeze)
	}
	price, err := subscriptionPrice(ctx, database, sub)
	if err != nil {
		return p, err
	}
	amountMinor, err := moneyutil.NativeToRailMinorExact(price.Currency, price.Amount)
	if err != nil {
		return p, fmt.Errorf("%w: price %s is not representable on the rail: %v", ErrNothingToFreeze, price.ID, err)
	}
	anchor, anchorSource := strings.TrimSpace(pm.StoredCredentialRecurringRef), "agreement"
	if anchor == "" {
		anchor, anchorSource = strings.TrimSpace(pm.InitialTransactionID), "legacy_initial_transaction_id"
	}
	if anchor == "" {
		anchorSource = "unavailable"
	}
	return intents.ManualRebillPayload{
		SubscriptionID: sub.ID, PeriodEnd: sub.CurrentPeriodEndsAt.UTC(), Rail: string(sub.Rail),
		OrderReference: OrderReference(sub), Attempt: AttemptOrdinal(sub),
		PaymentMethodID: pm.ID, CustomerVaultID: strings.TrimSpace(pm.RailCustomerRef),
		BillingID: strings.TrimSpace(pm.RailMethodRef), RailSubscriptionID: strings.TrimSpace(sub.RailSubscriptionID),
		CredentialReference: anchor, CredentialAnchorSource: anchorSource,
		Currency: price.Currency, Amount: price.Amount, AmountMinor: amountMinor,
	}, nil
}

// Window is the #839 dunning window for the subscription's billing cycle:
// past periodEnd+Window no rebill is ever charged.
func Window(ctx context.Context, database *db.DB, sub *models.Subscription) (time.Duration, error) {
	price, err := subscriptionPrice(ctx, database, sub)
	if err != nil {
		return 0, err
	}
	return collection.Window(collection.BillingCycleHoursOf(price)), nil
}

func subscriptionPrice(ctx context.Context, database *db.DB, sub *models.Subscription) (*models.Price, error) {
	if sub.Price != nil {
		return sub.Price, nil
	}
	price, err := catalog.NewPriceService(database).GetByID(ctx, sub.PriceID)
	if err != nil {
		return nil, fmt.Errorf("%w: load price: %v", ErrNothingToFreeze, err)
	}
	return price, nil
}
