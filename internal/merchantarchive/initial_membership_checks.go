package merchantarchive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

func validateInitialEnrollmentReferences(ctx context.Context, tx pgx.Tx, mid merchant.ID) error {
	q := gen.New(tx)
	var after *uuid.UUID
	for {
		rows, err := q.ListRetainedInitialEnrollmentsForArchive(ctx, gen.ListRetainedInitialEnrollmentsForArchiveParams{MerchantID: mid.UUID(), AfterID: after, PageSize: 256})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, op := range rows {
			if err := validateInitialEnrollmentReference(ctx, q, op); err != nil {
				return &Error{Code: "unsupported_state", Table: "rail_intents", Count: 1, Err: err}
			}
		}
		id := rows[len(rows)-1].ID
		after = &id
	}
}

func validateInitialEnrollmentReference(ctx context.Context, q *gen.Queries, op gen.OpenrailsRailIntent) error {
	if err := intents.ValidateInitialMembershipTerminal(op); err != nil {
		return err
	}
	p, err := subscriptions.DecodeInitialMembershipPayload(op)
	if err != nil {
		return err
	}
	var evidence struct {
		TransactionID          string `json:"transaction_id"`
		ProviderSubscriptionID string `json:"provider_subscription_id"`
		Declined               bool   `json:"declined"`
	}
	if err := json.Unmarshal(op.ResultEvidence, &evidence); err != nil {
		return err
	}
	row, subErr := q.GetInitialMembershipForArchive(ctx, gen.GetInitialMembershipForArchiveParams{MerchantID: op.MerchantID, ID: p.Terms.SubscriptionID})
	if op.Status == intents.StatusFailedTerminal || subErr == nil && row.Status == "pending" {
		hasGrant, err := q.HasInitialMembershipGrant(ctx, gen.HasInitialMembershipGrantParams{MerchantID: op.MerchantID, SubscriptionID: p.Terms.SubscriptionID})
		if err != nil {
			return err
		}
		if hasGrant {
			return errors.New("refused or pending initial membership has access grants")
		}
	}
	if op.Status == intents.StatusFailedTerminal {
		if subErr == nil {
			return errors.New("refused initial enrollment has a membership")
		}
		if !errors.Is(subErr, pgx.ErrNoRows) {
			return subErr
		}
		if p.Terms.PaymentID == uuid.Nil {
			return nil
		}
		payment, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: op.MerchantID, ID: p.Terms.PaymentID})
		if !evidence.Declined {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			return errors.New("unsent initial enrollment has a payment")
		}
		if err != nil {
			return err
		}
		refusal, _, err := intents.LoadInitialMembershipRefusal(op)
		if err != nil {
			return err
		}
		code := fmt.Sprint(refusal.Outcome().Evidence["response_code"])
		reason := payments.NormalizeFailureReason(op.Rail, code)
		if payment.SubscriptionID != nil || payment.ListAmount != p.Terms.RecurringAmount || payment.AttemptKind == nil || *payment.AttemptKind != payments.AttemptInitial || payment.FailureCode == nil || *payment.FailureCode != code || payment.FailureReason == nil || *payment.FailureReason != reason || !payment.PurchasedAt.Equal(p.Terms.AcceptedAt) || !payment.CreatedAt.Equal(p.Terms.AcceptedAt) {
			return errors.New("initial decline contradicts the sealed refusal and accepted attempt")
		}
		if payment.CustomerID != p.Terms.CustomerID || payment.PspID == nil || *payment.PspID != p.Terms.PSPID || payment.PriceID != p.Terms.PriceID || payment.Rail != op.Rail || payment.Amount != p.Terms.Amount || payment.Currency != p.Terms.Currency || payment.Status != "failed" || payment.MoneyMovement != "none" || payment.TransactionID != "nmi_sub_declined:"+op.ID.String() {
			return errors.New("initial decline points to another failed attempt")
		}
		return nil
	}
	if subErr != nil {
		return subErr
	}
	sub, err := models.SubscriptionFromGen(row)
	if err != nil {
		return err
	}
	if err := p.Terms.ValidateSubscriptionIdentity(sub, models.Rail(op.Rail), evidence.ProviderSubscriptionID); err != nil {
		if sub.ID != p.Terms.SubscriptionID || sub.CustomerID != p.Terms.CustomerID || sub.CollectionPolicy != p.Terms.CollectionPolicy || sub.Rail != models.Rail(op.Rail) || p.NativeSchedule == nil {
			return err
		}
		transitions, err := q.ListCompletedProviderCutoversForSubscription(ctx, gen.ListCompletedProviderCutoversForSubscriptionParams{MerchantID: op.MerchantID, SubscriptionID: sub.ID})
		if err != nil {
			return err
		}
		if err := intents.ValidateProviderCutoverLineage(op.MerchantID, sub.ID, sub.CustomerID, p.Terms.PSPID, evidence.ProviderSubscriptionID, sub.PspID, sub.RailSubscriptionID, transitions); err != nil {
			return err
		}
	}
	price, err := q.GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: op.MerchantID, ID: p.Terms.PriceID})
	if err != nil {
		return err
	}
	if price.ProductID != p.Terms.ProductID {
		return errors.New("initial accepted price belongs to another product")
	}
	if p.Terms.Amount > 0 {
		row, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: op.MerchantID, ID: p.Terms.PaymentID})
		if err != nil {
			return err
		}
		payment, err := models.PaymentFromGen(row)
		if err != nil {
			return err
		}
		if err := subscriptions.ValidateInitialMembershipPayment(p.Terms, payment, models.Rail(op.Rail), evidence.TransactionID); err != nil {
			return err
		}
	}
	historyTerms := p.Terms
	receipt, paid, err := intents.LoadCollectedReceipt(op)
	if err != nil {
		return err
	}
	if paid && receipt.ReversalKind() != "" {
		if sub.Status != models.StatusCancelled {
			return errors.New("reversed initial payment has an active agreement")
		}
		hasGrant, err := q.HasInitialMembershipGrant(ctx, gen.HasInitialMembershipGrantParams{MerchantID: op.MerchantID, SubscriptionID: sub.ID})
		if err != nil {
			return err
		}
		if hasGrant {
			return errors.New("reversed initial payment granted access")
		}
		historyTerms.Entitlements = map[string]*int{}
	}
	if p.Terms.Pending && sub.Status != models.StatusPending {
		anyGrant, err := q.HasInitialMembershipGrant(ctx, gen.HasInitialMembershipGrantParams{MerchantID: op.MerchantID, SubscriptionID: sub.ID})
		if err != nil {
			return err
		}
		observed, err := q.ListObservedInitialMembershipPayments(ctx, gen.ListObservedInitialMembershipPaymentsParams{MerchantID: op.MerchantID, SubscriptionID: sub.ID, OrderReference: intents.NMIEnrollmentOrder(op)})
		if err != nil {
			return err
		}
		if len(observed) > 1 {
			return errors.New("delayed initial membership has ambiguous first-payment history")
		}
		if p.Terms.RecurringAmount > 0 {
			if len(observed) == 0 {
				if anyGrant || sub.Status == models.StatusActive {
					return errors.New("delayed membership activated without its first payment")
				}
			} else {
				payment, err := models.PaymentFromGen(observed[0])
				if err != nil {
					return err
				}
				historyTerms.Pending = false
				historyTerms.Amount = historyTerms.RecurringAmount
				historyTerms.PaymentID = payment.ID
				if payment.TransactionID == "" || payment.PurchasedAt.Before(p.Terms.PeriodStart) {
					return errors.New("first scheduled payment precedes its accepted start")
				}
				if err := subscriptions.ValidateInitialMembershipPayment(historyTerms, payment, models.Rail(op.Rail), payment.TransactionID); err != nil {
					return err
				}
			}
		} else {
			if len(observed) != 0 {
				return errors.New("free initial phase carries an unexpected payment")
			}
			if anyGrant || sub.Status == models.StatusActive {
				historyTerms.Pending = false
			}
		}
	}
	before := historyTerms.PeriodEnd
	if historyTerms.Pending {
		before = historyTerms.PeriodStart
	}
	limit, err := safecast.Convert[int32](len(p.Terms.Entitlements) + 2)
	if err != nil {
		return err
	}
	history, err := q.ListInitialMembershipGrants(ctx, gen.ListInitialMembershipGrantsParams{MerchantID: op.MerchantID, SubscriptionID: p.Terms.SubscriptionID, Before: before, RowLimit: limit})
	if err != nil {
		return err
	}
	return subscriptions.ValidateInitialMembershipHistory(op.MerchantID, historyTerms, history)
}
