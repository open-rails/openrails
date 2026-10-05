package merchantarchive

import (
	"context"
	"errors"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// Accepted periods, payments and original grants are history. The current
// catalog, cancellation state and retained method are deliberately not authority
// for rewriting that history during an offline move.
func validateSubscriptionCollectionReferences(ctx context.Context, tx pgx.Tx, mid billing.MerchantID) error {
	q := gen.New(tx)
	var after *uuid.UUID
	for {
		rows, err := q.ListRetainedSubscriptionCollectionsForArchive(ctx, gen.ListRetainedSubscriptionCollectionsForArchiveParams{MerchantID: mid.UUID(), AfterID: after, PageSize: 256})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		for _, op := range rows {
			if err := validateSubscriptionCollectionReference(ctx, q, op); err != nil {
				return &Error{Code: "unsupported_state", Table: "provider_intents", Count: 1, Err: err}
			}
		}
		id := rows[len(rows)-1].ID
		after = &id
	}
	invalid, err := q.CountUnpaidEngineRenewalGrants(ctx, mid.UUID())
	if err != nil {
		return err
	}
	if invalid != 0 {
		return &Error{Code: "unsupported_state", Table: "grants", Count: invalid, Err: errors.New("engine renewal grants lack a paid accepted period")}
	}
	return nil
}

func validateSubscriptionCollectionReference(ctx context.Context, q *gen.Queries, op gen.BillingProviderIntent) error {
	if err := intents.ValidateSubscriptionCollectionTerminal(op); err != nil {
		return err
	}
	p, err := subscriptions.DecodeSubscriptionCollectionPayload(op)
	if err != nil {
		return err
	}
	t := p.Renewal
	sub, err := q.GetInitialMembershipForArchive(ctx, gen.GetInitialMembershipForArchiveParams{MerchantID: op.MerchantID, ID: t.SubscriptionID})
	if err != nil {
		return err
	}
	if sub.CustomerID != t.CustomerID || sub.CollectionPolicy != "engine" {
		return errors.New("engine collection names another membership")
	}
	price, err := q.GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: op.MerchantID, ID: t.PriceID})
	if err != nil {
		return err
	}
	if price.ProductID != t.ProductID {
		return errors.New("accepted renewal price belongs to another product")
	}
	receipt, paid, err := intents.LoadCollectedReceipt(op)
	if err != nil {
		return err
	}
	if !paid {
		// A declined or unsent renewal moved no money: it has no payment (a
		// decline is a payment attempt, #1111).
		return nil
	}
	transaction := receipt.TransactionID()
	row, err := q.GetPaymentByPSPTransactionID(ctx, gen.GetPaymentByPSPTransactionIDParams{MerchantID: op.MerchantID, PspID: op.PspID, Channel: "rail", Rail: &op.Rail, TransactionID: transaction})
	if err != nil {
		return err
	}
	payment, err := models.PaymentFromGen(row)
	if err != nil {
		return err
	}
	if payment.CreatedAt.Before(p.AcceptedAt) {
		return errors.New("engine payment predates its accepted obligation")
	}
	accepted := subscriptions.InitialMembershipTerms{PaymentID: payment.ID, SubscriptionID: t.SubscriptionID, CustomerID: t.CustomerID, PSPID: t.PSPID, PriceID: t.PriceID, ProductID: t.ProductID, Amount: t.Amount, RecurringAmount: t.Amount, Currency: t.Currency, Entitlements: t.Entitlements, PeriodStart: t.PeriodStart, PeriodEnd: t.PeriodEnd}
	if accepted.Entitlements == nil {
		accepted.Entitlements = map[string]*int{}
	}
	if err := subscriptions.ValidateInitialMembershipPayment(accepted, payment, models.Rail(op.Rail), transaction); err != nil {
		return err
	}
	if payment.AttemptKind == nil || *payment.AttemptKind != payments.AttemptRenewal || payment.TokenType == nil || *payment.TokenType != payments.DefaultTokenType(op.Rail, p.Instrument.Custodian) {
		return errors.New("engine payment lacks its recurring custody stamp")
	}
	limit, err := safecast.Convert[int32](len(t.Entitlements) + 2)
	if err != nil {
		return err
	}
	history, err := q.ListRenewalGrantsForArchive(ctx, gen.ListRenewalGrantsForArchiveParams{MerchantID: op.MerchantID, SubscriptionID: t.SubscriptionID, PeriodStart: t.PeriodStart, RowLimit: limit})
	if err != nil {
		return err
	}
	// Both paths are authored by the shared renewal writer in the same local
	// transaction as this payment: expired periods and refund-review payments do
	// not emit a fresh grant. A later cancellation only adds termination events.
	_, moneyOnly := payment.Metadata["refund_review"]
	if moneyOnly || !payment.CreatedAt.Before(t.PeriodEnd) {
		if len(history) != 0 {
			return errors.New("withheld engine renewal has access grants")
		}
		return nil
	}
	return subscriptions.ValidateInitialMembershipHistory(op.MerchantID, accepted, history)
}
