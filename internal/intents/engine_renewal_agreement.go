package intents

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// PrepareEngineRenewalTerms qualifies the exact paid predecessor under the
// admission transaction's subscription lock. A current catalog row is never
// evidence of what this customer accepted. Missing/pruned/ambiguous custody or
// a payment-only completion cannot authorize another recurring charge.
func PrepareEngineRenewalTerms(ctx context.Context, d *db.DB, sub *models.Subscription, now time.Time) (subscriptions.RenewalTerms, error) {
	var agreement subscriptions.RenewalTerms
	if d == nil || d.Pool() != nil || sub == nil || sub.CollectionPolicy != models.CollectionPolicyEngine || sub.CurrentPeriodEndsAt == nil {
		return agreement, fmt.Errorf("%w: engine agreement requires a locked current obligation", ErrRebillNotRetryable)
	}
	rows, err := d.Gen(ctx).ListPaidEngineAgreementsAtBoundary(ctx, gen.ListPaidEngineAgreementsAtBoundaryParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, PeriodEnd: *sub.CurrentPeriodEndsAt})
	if err != nil {
		return agreement, err
	}
	if len(rows) != 1 {
		return agreement, fmt.Errorf("%w: engine paid agreement is missing or ambiguous", ErrRebillNotRetryable)
	}
	op := rows[0]
	var accepted subscriptions.InitialMembershipTerms
	var payment gen.OpenrailsPayment
	switch op.IntentType {
	case subscriptions.TypeInitialMembership:
		if err := ValidateInitialMembershipTerminal(op); err != nil {
			return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
		}
		p, err := subscriptions.DecodeInitialMembershipPayload(op)
		if err != nil {
			return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
		}
		if p.Terms.CollectionPolicy != models.CollectionPolicyEngine || op.Origin != string(OriginUser) || op.Actor == nil || *op.Actor != p.Terms.CustomerID.String() {
			return agreement, fmt.Errorf("%w: engine initial agreement lacks its customer acceptance", ErrRebillNotRetryable)
		}
		accepted = p.Terms
		agreement = subscriptions.RenewalTerms{PSPID: accepted.PSPID, SubscriptionID: accepted.SubscriptionID, CustomerID: accepted.CustomerID, FromPriceID: accepted.PriceID, FromProductID: accepted.ProductID, PriceID: accepted.PriceID, ProductID: accepted.ProductID, ProductName: accepted.ProductName, Amount: accepted.RecurringAmount, Currency: accepted.Currency, PeriodStart: accepted.PeriodStart, PeriodEnd: accepted.PeriodEnd, Entitlements: accepted.Entitlements, PreviousEntitlements: accepted.Entitlements}
		payment, err = d.Gen(ctx).GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: op.MerchantID, ID: accepted.PaymentID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return agreement, fmt.Errorf("%w: paid agreement payment is missing", ErrRebillNotRetryable)
			}
			return agreement, err
		}
		if payment.AttemptKind == nil || *payment.AttemptKind != payments.AttemptInitial {
			return agreement, fmt.Errorf("%w: engine predecessor is not its initial payment", ErrRebillNotRetryable)
		}
	case subscriptions.TypeSubscriptionCollection:
		if err := ValidateSubscriptionCollectionTerminal(op); err != nil {
			return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
		}
		p, err := subscriptions.DecodeSubscriptionCollectionPayload(op)
		if err != nil {
			return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
		}
		agreement = p.Renewal
		receipt, _, err := LoadCollectedReceipt(op)
		if err != nil {
			return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
		}
		payment, err = d.Gen(ctx).GetPaymentByPSPTransactionID(ctx, gen.GetPaymentByPSPTransactionIDParams{MerchantID: op.MerchantID, PspID: op.PspID, Rail: op.Rail, TransactionID: receipt.TransactionID()})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return agreement, fmt.Errorf("%w: paid agreement payment is missing", ErrRebillNotRetryable)
			}
			return agreement, err
		}
		accepted = subscriptions.InitialMembershipTerms{PaymentID: payment.ID, PSPID: agreement.PSPID, SubscriptionID: agreement.SubscriptionID, CustomerID: agreement.CustomerID, PriceID: agreement.PriceID, ProductID: agreement.ProductID, Amount: agreement.Amount, RecurringAmount: agreement.Amount, Currency: agreement.Currency, Entitlements: models.CloneEntitlementsSpec(agreement.Entitlements)}
		if payment.AttemptKind == nil || *payment.AttemptKind != payments.AttemptRenewal {
			return agreement, fmt.Errorf("%w: engine predecessor is not a recurring payment", ErrRebillNotRetryable)
		}
	default:
		return agreement, fmt.Errorf("%w: engine predecessor is not an accepted membership operation", ErrRebillNotRetryable)
	}
	receipt, paid, err := LoadCollectedReceipt(op)
	if err != nil {
		return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
	}
	if !paid {
		return agreement, fmt.Errorf("%w: engine predecessor has no paid receipt", ErrRebillNotRetryable)
	}
	observed, err := models.PaymentFromGen(payment)
	if err != nil {
		return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
	}
	if _, moneyOnly := observed.Metadata["refund_review"]; moneyOnly {
		return agreement, fmt.Errorf("%w: payment-only completion is not a recurring agreement", ErrRebillNotRetryable)
	}
	if accepted.Entitlements == nil {
		accepted.Entitlements = map[string]*int{}
	}
	if err := subscriptions.ValidateInitialMembershipPayment(accepted, observed, models.Rail(op.Rail), receipt.TransactionID()); err != nil {
		return agreement, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
	}
	terms, err := subscriptions.PrepareRenewalTerms(ctx, d, sub, now, &agreement)
	if errors.Is(err, subscriptions.ErrEngineAgreementMismatch) {
		return terms, fmt.Errorf("%w: %w", ErrRebillNotRetryable, err)
	}
	return terms, err
}
