package subscriptions

// #815: gateway-native NMI auto-migration for #813 plan migrations.
//
// A gateway-native NMI recurring subscription (NMI initiates the rebills; no
// #297 stored-credential anchor) CAN be migrated server-side: classic Direct
// Post recurring=update_subscription mutates the remote record's plan_amount
// (merchant-initiated, vault-backed, no user interaction). NMI knows only
// amount + schedule — no product concept — so the rail-side flip for a plan
// change is plan_amount -> the target price's amount; the product/entitlement
// cutover stays internal.
//
// Timing is STRICTER than Stripe: NMI has no future-dated schedule object, so
// the push IS the next-rebill flip. Pushes happen only inside the change-
// boundary period (after the last old-price rebill, before the first
// new-price one) — see pushNMI's early-flip guard. Provider truth flips at
// push time (the remote record immediately reads the target amount, verified
// by read-back), so the internal cutover accompanies the push — mirroring the
// converge-from-provider-truth doctrine. Billing still flips only at the next
// rebill; the rebill date never moves; nothing is charged off-cycle;
// entitlement windows re-derive at the next renewal grant (#813 amendment).

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// NMIPusher is the #815 gateway-native NMI push seam (production impl:
// NewNMIPlanPusher over the #788 per-merchant client resolver; faked in
// tests). CanPush doubles as the NMI-family rail detector — it is resolver-
// driven, so custom-named NMI PSPs classify without code
// changes.
type NMIPusher interface {
	// CanPush reports whether sub is an addressable gateway-native NMI
	// recurring record: the merchant declares an armable NMI account for it
	// and the subscription carries a rail reference.
	CanPush(ctx context.Context, sub *models.Subscription) bool
	// PushPlanAmount sets the remote record's plan_amount to amountNative
	// (internal units at CURRENCY's registered scale, converted to NMI's
	// decimal form through the registry), PRESERVING the record's current
	// plan_payments, and read-back-verifies the flip took. It never touches
	// the schedule (day/month frequency), so the rebill date is unchanged.
	PushPlanAmount(ctx context.Context, sub *models.Subscription, currency string, amountNative int64) error
}

type nmiPlanPusher struct {
	resolver NMIClientSource
}

// NewNMIPlanPusher builds the production NMIPusher over the store-scoped NMI
// client resolver (#788; satisfied by money.MerchantCollectionAdapterBuilder).
func NewNMIPlanPusher(resolver NMIClientSource) NMIPusher {
	return &nmiPlanPusher{resolver: resolver}
}

func (p *nmiPlanPusher) CanPush(ctx context.Context, sub *models.Subscription) bool {
	if p == nil || p.resolver == nil || sub == nil {
		return false
	}
	if strings.TrimSpace(sub.RailSubscriptionID) == "" {
		return false
	}
	_, _, ok, err := NMIClientForExistingSubscription(ctx, p.resolver, sub)
	return err == nil && ok
}

func (p *nmiPlanPusher) PushPlanAmount(ctx context.Context, sub *models.Subscription, currency string, amountNative int64) error {
	client, _, ok, err := NMIClientForExistingSubscription(ctx, p.resolver, sub)
	if err != nil {
		return fmt.Errorf("nmi push: resolve client: %w", err)
	}
	if !ok || client == nil {
		return fmt.Errorf("nmi push: merchant declares no armable nmi account")
	}
	accountMerchant, accountPSP := client.AccountIdentity()
	if accountMerchant != sub.MerchantID || accountPSP != sub.PspID {
		return fmt.Errorf("nmi push: reader is armed for another provider account")
	}
	railID := strings.TrimSpace(sub.RailSubscriptionID)
	if railID == "" {
		return fmt.Errorf("nmi push: subscription missing nmi reference")
	}
	// or#863: through the registry, never an inline /10_000 — the converter is
	// the only thing here that knows the currency's scale, and the only thing
	// that can refuse an amount whose currency was never established.
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
		return fmt.Errorf("nmi push: exact subscription %s not found at nmi", railID)
	}
	// NMI ignores plan_amount on a schedule attached to a named plan; moving
	// one requires a plan swap, which a price migration does not name.
	if remote.NamedPlan() {
		return fmt.Errorf("nmi push: %s is on named plan %q; NMI changes its amount only by switching plans — migrate it by tier change to a linked plan", railID, remote.Plan.ID)
	}
	// update_subscription always writes the total installment count. Freeze an
	// explicit provider value; missing facts cannot silently become bill forever.
	if remote.Plan == nil || strings.TrimSpace(remote.Plan.PlanPayments) == "" {
		return fmt.Errorf("nmi push: %s has no qualified plan_payments", railID)
	}
	planPayments, err := strconv.Atoi(strings.TrimSpace(remote.Plan.PlanPayments))
	if err != nil || planPayments < 0 {
		return fmt.Errorf("nmi push: %s has unparseable plan_payments", railID)
	}

	wireAmount, err := nmi.WireAmount(cents, currency)
	if err != nil {
		return err
	}
	if _, err := client.UpdateRecurringSubscription(ctx, railID, wireAmount, planPayments); err != nil {
		return fmt.Errorf("nmi push: update %s: %w", railID, err)
	}

	// Converge-from-provider-truth: confirm the flip took before the caller
	// applies the internal cutover. A mismatch (or ambiguous update) leaves
	// the row blocked; a re-run re-pushes the same amount (a set-to-value op,
	// idempotent at NMI) and re-verifies.
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
		return fmt.Errorf("nmi push: update did not converge: remote amount %d rail units, want %d %s", gotCents, cents, currency)
	}
	return nil
}
