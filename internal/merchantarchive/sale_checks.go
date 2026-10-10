package merchantarchive

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/payments"
)

func validateSaleReferences(ctx context.Context, tx pgx.Tx, mid billing.MerchantID) error {
	q := gen.New(tx)
	var after *uuid.UUID
	for {
		rows, err := q.ListRetainedSalesForArchive(ctx, gen.ListRetainedSalesForArchiveParams{MerchantID: mid.UUID(), AfterID: after, PageSize: 256})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, op := range rows {
			if err := validateSaleReference(ctx, q, op); err != nil {
				return &Error{Code: "unsupported_state", Table: "provider_intents", Count: 1, Err: err}
			}
		}
		next := rows[len(rows)-1].ID
		after = &next
	}
}

func validateSaleReference(ctx context.Context, q *gen.Queries, op gen.BillingProviderIntent) error {
	if err := intents.ValidateNMISaleTerminal(op); err != nil {
		return err
	}
	p, err := payments.DecodeNMISalePayload(op)
	if err != nil {
		return err
	}
	var evidence struct {
		PaymentID     uuid.UUID `json:"payment_id"`
		TransactionID string    `json:"transaction_id"`
		Declined      bool      `json:"declined"`
	}
	if err := json.Unmarshal(op.ResultEvidence, &evidence); err != nil {
		return err
	}
	paymentID := evidence.PaymentID
	if op.Status == intents.StatusFailedTerminal {
		paymentID = p.PaymentID
	}
	observed, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: op.MerchantID, ID: paymentID})
	if op.Status == intents.StatusFailedTerminal {
		// A refused or unexecuted sale moved no money (a decline is a payment
		// attempt); only an older writer's decline record may carry its id.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if evidence.Declined && declineRecord(observed) {
			return nil
		}
		return errors.New("refused sale has a payment")
	}
	if err != nil {
		return err
	}
	customer, _ := uuid.Parse(p.UserID)
	bought := observed.PriceID != nil && *observed.PriceID == p.PriceID
	if p.OrderID != uuid.Nil {
		bought = observed.PriceID == nil && observed.OrderID != nil && *observed.OrderID == p.OrderID
	}
	if observed.MerchantID != op.MerchantID || observed.CustomerID != customer || observed.PspID == nil || *observed.PspID != p.Instrument.PSPID || !bought || observed.Rail == nil || *observed.Rail != op.Rail || observed.Amount != p.Amount || observed.ListAmount != p.ListAmount || observed.Currency != p.Currency || observed.SubscriptionID != nil {
		return errors.New("sale result points to another payment or commercial decision")
	}
	if !payments.PaymentStatusSucceeded(string(observed.Status)) || observed.MoneyMovement != "rail" || observed.TransactionID != evidence.TransactionID {
		return errors.New("sale result is not its exact completed payment")
	}
	if p.OrderID != uuid.Nil {
		return nil // The order's lines are what it bought.
	}
	price, err := q.GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: op.MerchantID, ID: p.PriceID})
	if err != nil {
		return err
	}
	if price.MerchantID != op.MerchantID || price.ProductID != p.ProductID {
		return errors.New("sale catalog identity contradicts its accepted product")
	}
	retained, err := models.PaymentFromGen(observed)
	if err != nil {
		return err
	}
	if !models.SameCreditGrantPromise(p.CreditGrant, retained.CreditGrantSnapshot) {
		return errors.New("sale payment has another credit promise")
	}
	original, err := q.ListOriginalPurchaseGrants(ctx, gen.ListOriginalPurchaseGrantsParams{MerchantID: op.MerchantID, PaymentID: paymentID, RowLimit: purchaseGrantLimit})
	if err != nil {
		return err
	}
	access := original[:0]
	for _, event := range original {
		if event.Kind != string(grants.Credit) {
			access = append(access, event)
		}
	}
	recorded, err := grants.RecordedPurchaseAccess(op.MerchantID, customer, p.ProductID, paymentID, access)
	if err != nil {
		return err
	}
	if recorded == nil {
		if p.CreditGrant == nil {
			return errors.New("terminal sale has incomplete original benefits")
		}
		return nil
	}
	// A grant converted from per-key windows keeps the windows' longest span.
	if !grants.Migrated(*recorded) && !grants.SameWindow(*recorded, grants.AccessWindow(p.AccessDurationHours, p.EntitlementStart)) {
		return errors.New("original access window contradicts accepted purchase")
	}
	return nil
}

// purchaseGrantLimit bounds one purchase's original grant events: its access
// grant and credit lot, plus per-key grants from before product access.
const purchaseGrantLimit = 10005

// declineRecord is a decline an older writer recorded in payments, not as a
// payment attempt: it moved no money.
func declineRecord(p gen.BillingPayment) bool {
	return p.Status == "failed" && p.MoneyMovement == "none" && p.RefundedPaymentID == nil
}
