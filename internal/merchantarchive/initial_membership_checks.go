package merchantarchive

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
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
		return err
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
	before := p.Terms.PeriodEnd
	if p.Terms.Pending {
		before = p.Terms.PeriodStart
	}
	limit, err := safecast.Convert[int32](len(p.Terms.Entitlements) + 2)
	if err != nil {
		return err
	}
	history, err := q.ListInitialMembershipGrants(ctx, gen.ListInitialMembershipGrantsParams{MerchantID: op.MerchantID, SubscriptionID: p.Terms.SubscriptionID, Before: before, RowLimit: limit})
	if err != nil {
		return err
	}
	return subscriptions.ValidateInitialMembershipHistory(op.MerchantID, p.Terms, history)
}
