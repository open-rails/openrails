package webhooks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// activateAcceptedInitialPayment consumes the provider's distinct first paid
// event after a no-charge enrollment. The terminal enrollment remains no-charge;
// current catalog edits cannot rewrite its accepted first period or benefits.
func (s *NMIConvergeService) activateAcceptedInitialPayment(ctx context.Context, sub *models.Subscription, now time.Time) (bool, error) {
	rows, err := s.DB.Gen(ctx).ListInitialEnrollmentsForMembership(ctx, gen.ListInitialEnrollmentsForMembershipParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID})
	if err != nil {
		return true, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	if len(rows) != 1 {
		return true, errors.New("pending membership has ambiguous accepted enrollment")
	}
	in := rows[0]
	p, err := subscriptions.DecodeNMIInitialEnrollmentPayload(in)
	if err != nil {
		return true, err
	}
	if !p.Terms.Pending {
		return true, fmt.Errorf("%w: paid-now membership belongs to its accepted enrollment completion", ErrConvergeRetryLater)
	}
	if in.Status != intents.StatusSucceeded {
		return true, fmt.Errorf("%w: initial schedule custody is not terminal", ErrConvergeRetryLater)
	}
	if err := intents.ValidateInitialEnrollmentTerminal(in); err != nil {
		return true, err
	}
	schedule, _, err := intents.LoadNMIEnrollmentReceipt(in)
	if err != nil {
		return true, err
	}
	if err := p.Terms.ValidateSubscriptionIdentity(sub, models.RailNMI, schedule.SubscriptionID()); err != nil {
		return true, err
	}
	if now.Before(p.Terms.PeriodStart) {
		return true, nil
	}
	if p.Terms.RecurringAmount == 0 {
		return true, s.activateInitialPhaseTx(ctx, in, p, schedule.SubscriptionID(), "", p.Terms.PeriodStart)
	}
	owner, account := s.NMIClient.AccountIdentity()
	if owner != in.MerchantID || account != p.Terms.PSPID {
		return true, errors.New("first scheduled payment reader differs from accepted account")
	}
	order := intents.NMIEnrollmentOrder(in)
	probe, err := s.NMIClient.ProbeSalesByOrderID(ctx, order, p.Terms.AcceptedAt.Truncate(time.Second))
	if err != nil {
		return true, err
	}
	if !probe.SuccessFound || probe.SuccessTransactionID == "" || probe.SuccessAt.IsZero() || probe.SuccessAt.Before(p.Terms.PeriodStart) || probe.SuccessAt.After(now) {
		return true, fmt.Errorf("%w: no dated first payment for accepted enrollment", ErrConvergeRetryLater)
	}
	paid, found, err := s.NMIClient.ReadSaleEvidence(ctx, order, probe.SuccessTransactionID)
	if err != nil {
		return true, err
	}
	if !found {
		return true, fmt.Errorf("%w: first scheduled payment is not qualified", ErrConvergeRetryLater)
	}
	minor, err := moneyutil.NativeToRailMinorExact(p.Currency, p.Terms.RecurringAmount)
	if err != nil {
		return true, err
	}
	if paid.Amount != minor || !strings.EqualFold(paid.Currency, p.Currency) || paid.CustomerVaultID != p.Instrument.RailCustomerRef {
		return true, errors.New("first scheduled payment contradicts frozen enrollment money or instrument")
	}
	if _, err := s.NMIClient.ReadSingleCardVaultBilling(ctx, p.Instrument.RailCustomerRef, p.Instrument.RailMethodRef); err != nil {
		return true, err
	}
	return true, s.activateInitialPhaseTx(ctx, in, p, schedule.SubscriptionID(), paid.TransactionID, probe.SuccessAt)
}

func (s *NMIConvergeService) activateInitialPhaseTx(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.NMIInitialEnrollmentPayload, providerRef, transaction string, purchasedAt time.Time) error {
	var notices []*models.NotificationQueue
	ctx = db.WithPSPID(ctx, p.Terms.PSPID)
	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.DB.NewWithPgxTx(tx)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: in.MerchantID, ID: p.Terms.CustomerID}); err != nil {
			return err
		}
		// This payment belongs to the observed provider event, not to the earlier
		// terminal no-charge enrollment. Existing PSP transaction uniqueness owns
		// replay; no new operation or provider submission is introduced.
		terms := p.Terms
		terms.Pending = false
		terms.Amount = terms.RecurringAmount
		terms.PaymentID = uuid.Nil
		if terms.Amount > 0 {
			terms.PaymentID = uuidutil.NewV7()
			existing, err := payments.NewPaymentService(d, s.Clock).GetByPSPTransactionID(ctx, models.RailNMI, transaction)
			if err != nil && !db.IsNotFound(err) {
				return err
			}
			if err == nil {
				terms.PaymentID = existing.ID
			}
		}
		var err error
		_, notices, err = s.SubscriptionLifecycleService.CreateMembershipTx(ctx, d, &subscriptions.CreateMembershipParams{Prepared: &terms, UserID: p.UserID, PriceID: p.PriceID, Rail: models.RailNMI, RailSubscriptionID: &providerRef, TransactionID: transaction, PurchasedAt: &purchasedAt, PaymentMetadata: map[string]any{"order_id": intents.NMIEnrollmentOrder(in), "provider_transaction_id": transaction}})
		// A provider-observed sale does not establish customer-initiated recurring
		// consent. Preserve any existing anchor; never infer one from this event.
		return err
	})
	if err == nil {
		s.SubscriptionLifecycleService.DispatchNotifications(ctx, notices)
	}
	return err
}
