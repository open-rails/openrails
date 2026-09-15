package money

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/pkg/identity"
)

// AdmissionCapture carries the shared receipt and original attribution for the
// service's optional usage event. It never accepts caller-supplied payer coordinates.
type AdmissionCapture struct {
	*openrails.CaptureReceipt
	Terms spendgate.Terms
}

func (s *MoneyService) CaptureAdmission(ctx context.Context, requestID string, amount int64) (*AdmissionCapture, error) {
	if amount < 0 {
		return nil, fmt.Errorf("captured amount must be nonnegative")
	}
	gate := spendgate.New(s.db)
	gate.SetClock(s.now)
	var result *AdmissionCapture
	err := gate.WithOperation(ctx, requestID, func(ctx context.Context, d *db.DB, row gen.OpenrailsAdmissionOperation) error {
		replayed := row.State == "captured"
		if replayed && (row.CapturedAmount == nil || *row.CapturedAmount != amount) {
			return &IdempotencyConflict{Operation: string(OpCapture), Source: "admit", SourceID: requestID,
				Field: "amount", Committed: derefInt(row.CapturedAmount), Retried: amount}
		}
		terms, err := spendgate.OriginalTerms(row)
		if err != nil {
			return err
		}
		if !replayed {
			// Remove this hold before spending, in the same transaction as the ledger.
			// A late actual after release/expiry still records the original operation.
			row, err = d.Gen(ctx).CaptureAdmissionOperation(ctx, gen.CaptureAdmissionOperationParams{
				MerchantID: row.MerchantID, RequestID: requestID, Amount: amount, AsOf: s.now(),
			})
			if err != nil {
				return err
			}
		}
		receipt := &openrails.CaptureReceipt{RequestID: requestID, CustomerID: row.PayerID, Currency: row.Currency, Amount: amount, Replayed: replayed}
		if amount > 0 {
			payer := identity.CustomerID(row.PayerID)
			transaction, err := NewMoneyService(d, s.clock).CaptureAuthorized(ctx, SpendParams{
				Payer: &payer, Invoker: terms.Invoker, Currency: row.Currency, Amount: amount,
				Key: MustIdempotencyKey(OpCapture, "admit", requestID),
			})
			if err != nil {
				return err
			}
			receipt.LedgerTransferID = &transaction.ID
		}
		result = &AdmissionCapture{CaptureReceipt: receipt, Terms: terms}
		return nil
	})
	return result, err
}
