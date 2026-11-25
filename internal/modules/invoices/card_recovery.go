package invoices

import (
	"context"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// restoreInvoiceCard completes card storage after the invoice already granted its
// paid access. The caller holds the customer and invoice locks. A later customer
// decision, including a revoked agreement, takes precedence over this retry.
func (s *Service) restoreInvoiceCard(ctx context.Context, d *db.DB, invoice *Invoice, paid *Charge, now time.Time) error {
	q := d.Gen(ctx)
	repo := subscriptions.NewSubscriptionRepo(d)
	restored := false
	for _, line := range invoice.Lines {
		if line.SubscriptionID == nil {
			continue
		}
		sub, err := repo.GetByIDForUpdate(ctx, *line.SubscriptionID)
		if err != nil {
			return err
		}
		if sub.CustomerID != invoice.CustomerID || sub.PspID != paid.PSPID || sub.CollectionPolicy != models.CollectionPolicyEngine || sub.Status != models.StatusAwaitingMethod || sub.PaymentMethodID != nil {
			continue
		}
		history, err := q.HasSubscriptionMandate(ctx, gen.HasSubscriptionMandateParams{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, SubscriptionID: sub.ID})
		if err != nil {
			return err
		}
		if history {
			continue
		}
		if _, err := mandates.Create(ctx, q, mandates.Agreement{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, PaymentMethodID: paid.PaymentMethodID, PSPID: paid.PSPID,
			Rail: paid.Rail, Kind: charge.AgreementRecurring, SubscriptionID: &sub.ID, Lineage: paid.lineage(), AcceptedAt: now}); err != nil {
			return err
		}
		sub.PaymentMethodID = &paid.PaymentMethodID
		if sub.CurrentPeriodEndsAt.After(now) {
			if err := entitlements.NewEntitlementService(d, s.Clock).RevokeSourcesForSubscriptionAsOf(ctx, sub.CustomerID.String(), sub.ID, now, models.AccessRevokeDunning, models.AccessSourceGrace); err != nil {
				return err
			}
			// This invoice already paid for the current period. Keeping its card
			// completes enrollment; it is not an overdue renewal.
			_, err = subscriptions.Transition(sub, lifecycle.Reinstate{PeriodStart: *sub.CurrentPeriodStartsAt, PeriodEnd: *sub.CurrentPeriodEndsAt}, now)
			sub.ClearRetrySchedule()
		} else {
			err = subscriptions.ReplaceMethod(sub, now)
		}
		if err != nil {
			return err
		}
		if err := repo.UpdateAt(ctx, sub, now); err != nil {
			return err
		}
		if err := subscriptions.WithdrawMethodNotice(ctx, d, sub.CustomerID, sub.ID); err != nil {
			return err
		}
		if err := s.Memberships.EnsureRenewalGrace(ctx, d, sub); err != nil {
			return err
		}
		restored = true
	}
	if restored && paid.Reuse {
		return mandates.RecordStored(ctx, q, mandates.Stored{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, PaymentMethodID: paid.PaymentMethodID, PSPID: paid.PSPID,
			Rail: paid.Rail, Currency: invoice.Currency, Lineage: *paid.lineage(), AcceptedAt: now})
	}
	return nil
}
