package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
)

// CaptureAdmission commits the actual charge, immutable capture terms, and any
// usage event together. Payer, invoker, and unit come from the original admission.
func (s *MoneyService) CaptureAdmission(ctx context.Context, requestID string, amount int64, usage *openrails.CaptureUsage) (*openrails.CaptureReceipt, error) {
	if amount < 0 {
		return nil, fmt.Errorf("captured amount must be nonnegative")
	}
	var u openrails.CaptureUsage
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
	var result *openrails.CaptureReceipt
	err = gate.WithOperation(ctx, requestID, func(ctx context.Context, d *db.DB, row gen.OpenrailsAdmissionOperation) error {
		replayed := row.State == "captured"
		if replayed {
			if row.CapturedAmount == nil || *row.CapturedAmount != amount {
				return &IdempotencyConflict{Operation: string(OpCapture), Source: "admit", SourceID: requestID,
					Field: "amount", Committed: derefInt(row.CapturedAmount), Retried: amount}
			}
			matches, err := d.Gen(ctx).AdmissionCaptureTermsMatch(ctx, gen.AdmissionCaptureTermsMatchParams{
				MerchantID: row.MerchantID, RequestID: requestID, CaptureTerms: captureTerms,
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
				MerchantID: row.MerchantID, RequestID: requestID, Amount: amount, AsOf: s.now(), CaptureTerms: captureTerms,
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
			err = d.Gen(ctx).InsertUsageEvent(ctx, gen.InsertUsageEventParams{
				ID: uuidutil.NewV7(), MerchantID: row.MerchantID, CustomerID: row.PayerID,
				InvokerID: terms.Invoker, Currency: row.Currency, Resource: nilIfEmpty(u.Resource),
				EventType: u.EventType, Dimensions: dimensions, Metadata: metadata, Amount: amount, PricingAuthority: "host",
				Source: u.Source, SourceID: u.SourceID, LedgerTransferID: receipt.LedgerTransferID,
				OccurredAt: now, CreatedAt: now,
			})
			if err != nil {
				var conflict *pgconn.PgError
				if errors.As(err, &conflict) && conflict.Code == "23505" && conflict.ConstraintName == "uq_usage_events_idem" {
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
