package intents

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// rebillPaymentAlreadyObserved runs under the subscription admission lock.
// Accepted operation intervals govern their payments regardless of when local
// recovery wrote PurchasedAt. Unattributed provider observations retain only the
// existing conservative timestamp refusal, not a claim of exact coverage.
func rebillPaymentAlreadyObserved(ctx context.Context, d *db.DB, merchantID uuid.UUID, target ManualRebillPayload) (bool, error) {
	rows, err := d.Gen(ctx).ListCompletedManualRebillPaymentCoverage(ctx, gen.ListCompletedManualRebillPaymentCoverageParams{MerchantID: merchantID, SubscriptionID: target.Renewal.SubscriptionID, PspID: target.Instrument.PSPID})
	if err != nil {
		return false, err
	}
	covered := false
	for _, row := range rows {
		overlaps, err := acceptedRebillPaymentOverlaps(row, target)
		if err != nil {
			return false, err
		}
		covered = covered || overlaps
	}
	if covered {
		return true, nil
	}
	return d.Gen(ctx).HasUnattributedPaymentAfterRebillBoundary(ctx, gen.HasUnattributedPaymentAfterRebillBoundaryParams{MerchantID: merchantID, SubscriptionID: target.Renewal.SubscriptionID, PspID: target.Instrument.PSPID, PeriodStart: target.Renewal.PeriodStart})
}

func acceptedRebillPaymentOverlaps(row gen.ListCompletedManualRebillPaymentCoverageRow, target ManualRebillPayload) (bool, error) {
	in := row.OpenrailsRailIntent
	accepted, err := DecodeManualRebillPayload(in)
	if err != nil {
		return false, err
	}
	receipt, found, err := LoadCollectedReceipt(in)
	if err != nil {
		return false, err
	}
	if !found {
		return false, errors.New("rebill payment has no qualified receipt custody")
	}
	if accepted.Renewal.CustomerID != target.Renewal.CustomerID || accepted.Renewal.SubscriptionID != target.Renewal.SubscriptionID || accepted.Instrument.PSPID != target.Instrument.PSPID || row.PaidCustomerID != accepted.Renewal.CustomerID || row.PaidSubscriptionID == nil || *row.PaidSubscriptionID != accepted.Renewal.SubscriptionID || row.PaidPspID == nil || *row.PaidPspID != accepted.Instrument.PSPID || row.PaidRail != accepted.Rail || row.PaidTransactionID != receipt.TransactionID() || row.PaidPriceID != accepted.Renewal.PriceID || row.PaidAmount != accepted.Renewal.Amount || row.PaidCurrency != accepted.Renewal.Currency {
		return false, errors.New("completed rebill payment contradicts its accepted receipt terms")
	}
	return accepted.Renewal.PeriodStart.Before(target.Renewal.PeriodEnd) && target.Renewal.PeriodStart.Before(accepted.Renewal.PeriodEnd), nil
}
