package subscriptions

import (
	"errors"
	"time"

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
	if p == nil || p.ID != t.PaymentID || p.CustomerID != t.CustomerID || p.PriceID != t.PriceID || p.PspID == nil || *p.PspID != t.PSPID || p.SubscriptionID == nil || *p.SubscriptionID != t.SubscriptionID || p.Rail != rail || p.TransactionID != transaction || p.Amount != t.Amount || p.ListAmount != t.RecurringAmount || p.Currency != t.Currency || !payments.PaymentStatusSucceeded(p.Status) || p.MoneyMovement != models.MoneyMovementRail {
		return errors.New("existing initial payment contradicts accepted enrollment")
	}
	return nil
}

// InitialMembershipGrantLimit bounds the initial period's history read: one
// access grant, plus per-key grants from before product access.
const InitialMembershipGrantLimit = 10002

// ValidateInitialMembershipHistory reads immutable source grants. It deliberately
// ignores mutable current periods and later revoke records, and never restores
// a projection merely because an old accepted operation is replayed. The
// initial period grants its product once; per-key grants from before product
// access are superseded history.
func ValidateInitialMembershipHistory(merchant uuid.UUID, t InitialMembershipTerms, rows []gen.BillingGrant) error {
	if t.Pending {
		if len(rows) != 0 {
			return errors.New("pending enrollment granted access before its accepted start")
		}
		return nil
	}
	access := 0
	for _, g := range rows {
		if g.MerchantID != merchant || g.CustomerID != t.CustomerID || g.SourceType != string(grants.Subscription) || models.DerefStr(g.SourceID) != t.SubscriptionID.String() || g.Event != "grant" || g.SupersedesID != nil || g.ProductID != nil && *g.ProductID != t.ProductID || g.PaymentID != nil && *g.PaymentID != t.PaymentID || !g.StartsAt.Equal(t.PeriodStart) || !sameAccessEnd(g.EndsAt, accessEnd(t.PeriodStart, t.AccessDurationHours)) {
			return errors.New("initial membership grant has another owner or interval")
		}
		switch g.Kind {
		case string(grants.Access):
			access++
		case string(grants.Entitlement):
		default:
			return errors.New("initial membership grant has another benefit kind")
		}
	}
	if access > 1 {
		return errors.New("initial membership granted its product twice")
	}
	return nil
}

func sameAccessEnd(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
