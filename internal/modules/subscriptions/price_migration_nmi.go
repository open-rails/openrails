package subscriptions

// A gateway-native NMI recurring subscription (NMI initiates the rebills) is
// migrated server-side: update_subscription sets the remote plan_amount to the
// target price's amount. NMI knows only amount and schedule, so the product
// change stays internal.
//
// NMI has no future-dated schedule, so the push is the next-rebill flip: it
// happens only in the period before the first new-price rebill (pushNMI's
// early-flip guard). The internal move accompanies the read-back-verified push;
// the rebill date never moves and nothing is charged off-cycle.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// NMIPlanPusher pushes a migration to an NMI gateway schedule. CanPush doubles
// as the NMI-family rail detector, so custom-named NMI PSPs classify without
// code changes.
type NMIPlanPusher struct {
	resolver railresolve.NMIClientResolver
}

// NewNMIPlanPusher builds the pusher over the merchant's NMI client resolver.
func NewNMIPlanPusher(resolver railresolve.NMIClientResolver) *NMIPlanPusher {
	return &NMIPlanPusher{resolver: resolver}
}

// CanPush reports whether sub is an addressable gateway-native NMI recurring
// record: the merchant declares an armable NMI account for it and the
// subscription carries a rail reference. An error is a read that failed,
// which says nothing either way.
func (p *NMIPlanPusher) CanPush(ctx context.Context, sub *models.Subscription) (bool, error) {
	if p == nil || p.resolver == nil || sub == nil || strings.TrimSpace(sub.RailSubscriptionID) == "" {
		return false, nil
	}
	_, _, ok, err := NMIClientForExistingSubscription(ctx, p.resolver, sub)
	return ok, err
}

// PushPlanAmount sets the remote record's plan_amount to amountNative
// (internal units at currency's registered scale, converted to NMI's decimal
// form through the registry), PRESERVING the record's current plan_payments,
// and read-back-verifies the flip took. It never touches the schedule
// (day/month frequency), so the rebill date is unchanged.
func (p *NMIPlanPusher) PushPlanAmount(ctx context.Context, sub *models.Subscription, currency string, amountNative int64) error {
	client, _, ok, err := NMIClientForExistingSubscription(ctx, p.resolver, sub)
	if err != nil {
		return fmt.Errorf("nmi push: resolve client: %w", err)
	}
	if !ok || client == nil {
		return refusePush(migrationBlockedRailUserAction, "the merchant declares no armable nmi account")
	}
	accountMerchant, accountPSP := client.AccountIdentity()
	if accountMerchant != sub.MerchantID || accountPSP != sub.PspID {
		return refusePush(migrationBlockedRailUserAction, "the nmi reader is armed for another account")
	}
	railID := strings.TrimSpace(sub.RailSubscriptionID)
	if railID == "" {
		return fmt.Errorf("nmi push: subscription missing nmi reference")
	}
	// Through the registry, never an inline /10_000: only the converter knows
	// the currency's scale and refuses an unestablished currency.
	cents, err := moneyutil.NativeToRailMinorExact(currency, amountNative)
	if err != nil {
		return fmt.Errorf("nmi push: %w", err)
	}

	// Read the current record FIRST: existence check + plan_payments
	// preservation (update_subscription always sends plan_payments; resetting
	// it would rewrite a finite-payments schedule).
	remote, found, err := client.GetSubscription(ctx, railID)
	if err != nil {
		return fmt.Errorf("nmi push: read %s: %w", railID, err)
	}
	if !found || remote.ID != railID {
		return refusePush(migrationBlockedNMIScheduleMissing, "nmi has no schedule %s", railID)
	}
	// NMI ignores plan_amount on a schedule attached to a named plan; moving
	// one requires a plan swap, which a price migration does not name.
	if remote.NamedPlan() {
		return refusePush(migrationBlockedNMINamedPlan, "schedule %s is on plan %q", railID, remote.Plan.ID)
	}
	// update_subscription always writes the total installment count. Freeze an
	// explicit provider value; missing facts cannot silently become bill forever.
	planPayments := -1
	if remote.Plan != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(remote.Plan.PlanPayments)); err == nil {
			planPayments = n
		}
	}
	if planPayments < 0 {
		return refusePush(migrationBlockedNMIPaymentsUnknown, "schedule %s", railID)
	}

	wireAmount, err := nmi.WireAmount(cents, currency)
	if err != nil {
		return err
	}
	if _, err := client.UpdateRecurringSubscription(ctx, railID, wireAmount, planPayments); err != nil {
		return fmt.Errorf("nmi push: update %s: %w", railID, err)
	}

	// Confirm the flip took before the caller moves the subscription. A
	// mismatch (or ambiguous update) leaves the move blocked; a re-run
	// re-pushes the same amount (idempotent at NMI) and re-verifies.
	after, found, err := client.GetSubscription(ctx, railID)
	if err != nil {
		return fmt.Errorf("nmi push: verify %s: %w", railID, err)
	}
	if !found || after.ID != railID {
		return fmt.Errorf("nmi push: exact subscription %s vanished during update", railID)
	}
	gotCents, err := nmi.SubscriptionAmountMinor(after, currency)
	if err != nil {
		return fmt.Errorf("nmi push: verify %s: %w", railID, err)
	}
	if gotCents != cents {
		return fmt.Errorf("nmi push: update did not converge: remote amount %s, want %s", moneyutil.FormatRailMinor(gotCents, currency), moneyutil.FormatRailMinor(cents, currency))
	}
	return nil
}
