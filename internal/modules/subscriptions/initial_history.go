package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// validateSolanaEnrollmentOrder proves the first wallet payment from the
// immutable paid order; membership fulfillment must not record it twice.
func validateSolanaEnrollmentOrder(ctx context.Context, q *gen.Queries, mid uuid.UUID, t InitialMembershipTerms, orderID *uuid.UUID, providerRef string) error {
	if orderID == nil || t.Amount != 0 || t.PaymentID != uuid.Nil {
		return errors.New("Solana enrollment requires its already paid order")
	}
	o, err := q.GetOrder(ctx, gen.GetOrderParams{MerchantID: mid, ID: *orderID})
	if err != nil {
		return err
	}
	if o.CustomerID != t.CustomerID || o.Status != "complete" || o.PaymentStatus != "succeeded" || o.PaymentID == nil || o.Total != t.RecurringAmount || o.Currency != t.Currency {
		return errors.New("Solana enrollment contradicts its paid order")
	}
	p, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid, ID: *o.PaymentID})
	if err != nil {
		return err
	}
	if p.CustomerID != t.CustomerID || p.OrderID == nil || *p.OrderID != o.ID || p.PspID == nil || *p.PspID != t.PSPID || models.DerefStr(p.Rail) != string(models.RailSolana) || p.TransactionID == "" || p.Amount != o.Total || p.Currency != o.Currency || !payments.PaymentStatusSucceeded(p.Status) || p.MoneyMovement != string(models.MoneyMovementRail) {
		return errors.New("Solana enrollment has no matching successful wallet payment")
	}
	var receipt struct {
		SubscriptionPDA string `json:"solana_subscription_pda"`
	}
	if json.Unmarshal(p.Metadata, &receipt) != nil || providerRef == "" || receipt.SubscriptionPDA != providerRef || !p.PurchasedAt.Equal(t.PeriodStart) {
		return errors.New("Solana enrollment contradicts its verified subscription receipt")
	}
	lines, err := q.ListOrderLines(ctx, gen.ListOrderLinesParams{MerchantID: mid, OrderIds: []uuid.UUID{o.ID}})
	if err != nil {
		return err
	}
	if len(lines) != 1 || lines[0].PriceID == nil || *lines[0].PriceID != t.PriceID || lines[0].ProductID == nil || *lines[0].ProductID != t.ProductID || lines[0].Amount != t.RecurringAmount || lines[0].BillingIntervalHours == nil || t.RecurringIntervalHours == nil || int(*lines[0].BillingIntervalHours) != *t.RecurringIntervalHours {
		return errors.New("Solana enrollment contradicts its accepted order line")
	}
	return nil
}

func (t InitialMembershipTerms) ValidateSubscriptionIdentity(sub *models.Subscription, rail models.Rail, providerRef string) error {
	if sub == nil || sub.ID != t.SubscriptionID || sub.CustomerID != t.CustomerID || sub.PspID != t.PSPID || sub.CollectionPolicy != t.CollectionPolicy || sub.Rail != rail || sub.RailSubscriptionID != providerRef {
		return errors.New("existing membership belongs to another accepted enrollment")
	}
	return nil
}

// ValidateInitialMembershipPayment checks original paid facts, not a current
// catalog projection. Later membership changes cannot change this payment.
func ValidateInitialMembershipPayment(t InitialMembershipTerms, p *models.Payment, rail models.Rail, transaction string) error {
	if p == nil || p.ID != t.PaymentID || p.CustomerID != t.CustomerID || p.PspID == nil || *p.PspID != t.PSPID || p.SubscriptionID == nil || *p.SubscriptionID != t.SubscriptionID || p.Rail != rail || p.TransactionID != transaction || p.Amount != t.Amount || p.ListAmount != t.RecurringAmount || p.Currency != t.Currency || !payments.PaymentStatusSucceeded(p.Status) || p.MoneyMovement != models.MoneyMovementRail {
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
	acceptedEnd := accessEnd(t.PeriodStart, t.AccessDurationHours)
	if t.ProviderAccessDefault {
		acceptedEnd = &t.PeriodEnd
	}
	access := 0
	for _, g := range rows {
		if g.MerchantID != merchant || g.CustomerID != t.CustomerID || g.SourceType != string(grants.Subscription) || models.DerefStr(g.SourceID) != t.SubscriptionID.String() || g.Event != "grant" || g.SupersedesID != nil || g.ProductID != nil && *g.ProductID != t.ProductID || g.PaymentID != nil && *g.PaymentID != t.PaymentID || !g.StartsAt.Equal(t.PeriodStart) || !sameAccessEnd(g.EndsAt, acceptedEnd) {
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
