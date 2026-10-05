package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// CaptureAdmission commits the actual charge, immutable capture terms, and any
// usage event together. Payer, invoker, and unit come from the original admission.
func (s *MoneyService) CaptureAdmission(ctx context.Context, requestID string, amount int64, usage *billing.CaptureUsage) (*billing.CaptureReceipt, error) {
	if amount < 0 {
		return nil, fmt.Errorf("captured amount must be nonnegative")
	}
	var u billing.CaptureUsage
	if usage != nil {
		u = *usage
	}
	u.EventType = strings.TrimSpace(u.EventType)
	u.Resource = strings.TrimSpace(u.Resource)
	u.Source = strings.TrimSpace(u.Source)
	u.SourceID = strings.TrimSpace(u.SourceID)
	if u.EventType != "" {
		if u.Source == "" {
			u.Source = "admit"
		}
		if u.SourceID == "" {
			u.SourceID = requestID
		}
	}
	captureTerms, err := json.Marshal(u)
	if err != nil {
		return nil, fmt.Errorf("capture usage terms: %w", err)
	}
	gate := spendgate.New(s.db)
	gate.SetClock(s.now)
	s.db.EnsurePartitions(ctx, s.now())
	var result *billing.CaptureReceipt
	err = gate.WithOperation(ctx, requestID, func(ctx context.Context, d *db.DB, row gen.BillingAdmissionOperation) error {
		replayed := row.State == "captured"
		if replayed {
			if row.CapturedAmount == nil || *row.CapturedAmount != amount {
				return &IdempotencyConflict{Operation: string(OpCapture), Source: "admit", SourceID: requestID,
					Field: "amount", Committed: derefInt(row.CapturedAmount), Retried: amount}
			}
			matches, err := d.Gen(ctx).AdmissionCaptureTermsMatch(ctx, gen.AdmissionCaptureTermsMatchParams{
				MerchantID: row.MerchantID, RequestID: requestID, AdmittedAt: row.AdmittedAt, CaptureTerms: captureTerms,
			})
			if err != nil {
				return err
			}
			if !matches {
				return &IdempotencyConflict{Operation: string(OpCapture), Source: "admit", SourceID: requestID,
					Field: "usage", Committed: "original capture terms", Retried: "different capture terms"}
			}
		}
		terms, err := spendgate.OriginalTerms(row)
		if err != nil {
			return err
		}
		if !replayed {
			// Remove this hold before spending, in the same transaction as the ledger.
			// A late actual after release/expiry still records the original operation.
			row, err = d.Gen(ctx).CaptureAdmissionOperation(ctx, gen.CaptureAdmissionOperationParams{
				MerchantID: row.MerchantID, RequestID: requestID, AdmittedAt: row.AdmittedAt, Amount: amount, AsOf: s.now(), CaptureTerms: captureTerms,
			})
			if err != nil {
				return err
			}
		}
		receipt := &billing.CaptureReceipt{RequestID: requestID, CustomerID: billing.CustomerID(row.CustomerID), Currency: row.Currency, Amount: amount, Replayed: replayed}
		var ledgerTransferID *uuid.UUID
		if amount > 0 {
			payer := identity.CustomerID(row.CustomerID)
			transaction, err := NewMoneyService(d, s.clock).CaptureAuthorized(ctx, SpendParams{
				Payer: &payer, Invoker: terms.Invoker, Currency: row.Currency, Amount: amount,
				Key: MustIdempotencyKey(OpCapture, "admit", requestID),
			})
			if err != nil {
				return err
			}
			ledgerTransferID = &transaction.ID
			txn := billing.CreditTransactionID(transaction.ID)
			receipt.CreditTransactionID = &txn
		}
		if !replayed && u.EventType != "" {
			dimensions, err := toJSONBC(u.Dimensions)
			if err != nil {
				return err
			}
			metadata, err := toJSONBC(u.Metadata)
			if err != nil {
				return err
			}
			now := s.now()
			// The coordinate is claimed here, under the customer spend lock.
			keyFrom, keyTo := usageKeyWindow(now)
			_, err = d.Gen(ctx).GetUsageEventByCoords(ctx, gen.GetUsageEventByCoordsParams{
				MerchantID: row.MerchantID, CustomerID: row.CustomerID, Currency: row.Currency,
				EventType: u.EventType, Source: u.Source, SourceID: u.SourceID,
				OccurredFrom: keyFrom, OccurredTo: keyTo,
			})
			if err == nil {
				return fmt.Errorf("%w: capture usage coordinate belongs to another operation", ErrIdempotencyKeyReused)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			err = d.Gen(ctx).InsertUsageEvent(ctx, gen.InsertUsageEventParams{
				ID: uuidutil.NewV7(), MerchantID: row.MerchantID, CustomerID: row.CustomerID,
				InvokerID: terms.Invoker, Currency: row.Currency, Resource: nilIfEmpty(u.Resource),
				EventType: u.EventType, Dimensions: dimensions, Metadata: metadata, Amount: amount, PricingAuthority: "host",
				Source: u.Source, SourceID: u.SourceID, LedgerTransferID: ledgerTransferID,
				OccurredAt: now, CreatedAt: now,
			})
			if err != nil {
				var conflict *pgconn.PgError
				if errors.As(err, &conflict) && conflict.Code == "23505" && conflict.ConstraintName == "usage_events_idem_key" {
					return fmt.Errorf("%w: capture usage coordinate belongs to another operation", ErrIdempotencyKeyReused)
				}
				return err
			}
		}
		result = receipt
		return nil
	})
	return result, err
}
