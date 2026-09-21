package subscriptions

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/payments"
)

func (t InitialMembershipTerms) ValidateSubscriptionIdentity(sub *models.Subscription, rail models.Rail, providerRef string) error {
	if sub == nil || sub.ID != t.SubscriptionID || sub.CustomerID != t.CustomerID || sub.PspID != t.PSPID || sub.CollectionPolicy != t.CollectionPolicy || sub.Rail != rail || sub.RailSubscriptionID != providerRef {
		return errors.New("existing membership belongs to another accepted enrollment")
	}
	return nil
}

// ValidateInitialMembershipPayment checks original paid facts, not a current
// catalog projection. Later membership changes cannot change this payment.
func ValidateInitialMembershipPayment(t InitialMembershipTerms, p *models.Payment, rail models.Rail, transaction string) error {
	if p == nil || p.ID != t.PaymentID || p.CustomerID != t.CustomerID || p.PriceID != t.PriceID || p.PspID == nil || *p.PspID != t.PSPID || p.SubscriptionID == nil || *p.SubscriptionID != t.SubscriptionID || p.Rail != rail || p.TransactionID != transaction || p.Amount != t.Amount || p.ListAmount != t.RecurringAmount || p.Currency != t.Currency || !payments.PaymentStatusCompleted(p.Status) || p.MoneyMovement != models.MoneyMovementRail {
		return errors.New("existing initial payment contradicts accepted enrollment")
	}
	snapshot := p.EntitlementsSpecSnapshot
	if snapshot == nil {
		snapshot = map[string]*int{}
	}
	if !reflect.DeepEqual(snapshot, t.Entitlements) {
		return errors.New("initial payment has another accepted benefit snapshot")
	}
	return nil
}

// ValidateInitialMembershipHistory reads immutable source grants. It deliberately
// ignores mutable current periods and later revoke records, and never restores
// a projection merely because an old accepted operation is replayed.
func ValidateInitialMembershipHistory(merchant uuid.UUID, t InitialMembershipTerms, rows []gen.OpenrailsGrant) error {
	if t.Pending {
		if len(rows) != 0 {
			return errors.New("pending enrollment granted access before its accepted start")
		}
		return nil
	}
	seen := map[string]bool{}
	for _, g := range rows {
		if g.MerchantID != merchant || g.CustomerID != t.CustomerID || g.Kind != string(grants.Entitlement) || g.SourceType != string(grants.Subscription) || g.SourceID != t.SubscriptionID.String() || g.Event != "grant" || g.SupersedesID != nil || g.ProductID != nil && *g.ProductID != t.ProductID || g.PaymentID != nil && *g.PaymentID != t.PaymentID || !g.StartsAt.Equal(t.PeriodStart) || g.EndsAt == nil || !g.EndsAt.Equal(t.PeriodEnd) {
			return errors.New("initial membership grant has another owner or interval")
		}
		var spec grants.Spec
		if err := json.Unmarshal(g.SpecSnapshot, &spec); err != nil || len(spec.Entitlements) == 0 || spec.Deposit != nil {
			return errors.New("initial membership grant has another benefit kind")
		}
		for _, name := range spec.Entitlements {
			if _, ok := t.Entitlements[name]; !ok || seen[name] {
				return errors.New("initial membership grants contradict accepted benefits")
			}
			seen[name] = true
		}
	}
	if len(seen) != len(t.Entitlements) {
		return errors.New("initial membership has incomplete immutable grant history")
	}
	return nil
}
