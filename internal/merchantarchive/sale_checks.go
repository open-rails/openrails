package merchantarchive

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/merchant"
)

func validateSaleReferences(ctx context.Context, tx pgx.Tx, mid merchant.ID) error {
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
				return &Error{Code: "unsupported_state", Table: "rail_intents", Count: 1, Err: err}
			}
		}
		next := rows[len(rows)-1].ID
		after = &next
	}
}

func validateSaleReference(ctx context.Context, q *gen.Queries, op gen.OpenrailsRailIntent) error {
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
	observed, err := q.GetPaymentByID(ctx, paymentID)
	if op.Status == intents.StatusFailedTerminal && !evidence.Declined {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return errors.New("unexecuted sale has a payment")
	}
	if err != nil {
		return err
	}
	customer, _ := uuid.Parse(p.UserID)
	if observed.MerchantID != op.MerchantID || observed.CustomerID != customer || observed.PspID == nil || *observed.PspID != p.Instrument.PSPID || observed.PriceID != p.PriceID || observed.Rail != "nmi" || observed.Amount != p.Amount || observed.ListAmount != p.ListAmount || observed.Currency != p.Currency || observed.SubscriptionID != nil {
		return errors.New("sale result points to another payment or commercial decision")
	}
	if op.Status == intents.StatusFailedTerminal {
		if observed.Status != "failed" || observed.MoneyMovement != "none" || observed.TransactionID != "nmi_sale_declined:"+op.ID.String() {
			return errors.New("sale decline points to another attempt")
		}
		return nil
	}
	if !payments.PaymentStatusCompleted(string(observed.Status)) || observed.MoneyMovement != "rail" || observed.TransactionID != evidence.TransactionID {
		return errors.New("sale result is not its exact completed payment")
	}
	price, err := q.GetPriceByID(ctx, p.PriceID)
	if err != nil {
		return err
	}
	if price.MerchantID != op.MerchantID || price.ProductID != p.ProductID {
		return errors.New("sale catalog identity contradicts its accepted product")
	}
	snapshot := map[string]*int{}
	if len(observed.EntitlementsSpecSnapshot) > 0 {
		if err := json.Unmarshal(observed.EntitlementsSpecSnapshot, &snapshot); err != nil {
			return err
		}
	}
	if snapshot == nil {
		snapshot = map[string]*int{}
	}
	if !reflect.DeepEqual(snapshot, p.Entitlements) {
		return errors.New("sale payment has another benefit snapshot")
	}
	original, err := q.ListOriginalPurchaseGrants(ctx, gen.ListOriginalPurchaseGrantsParams{MerchantID: op.MerchantID, PaymentID: paymentID, RowLimit: int32(len(p.Entitlements) + 2)})
	if err != nil {
		return err
	}
	windows, ownership := grants.PurchaseWindows(p.Entitlements, p.AccessDurationHours, p.AcceptedAt, p.EntitlementStart)
	history, err := grants.ValidatePurchaseHistory(op.MerchantID, customer, p.ProductID, paymentID, windows, ownership, original)
	if err != nil {
		return err
	}
	if history.Ownership == nil || len(history.Entitlements) != len(windows) {
		return errors.New("terminal sale has incomplete original benefits")
	}
	return nil
}
