package money

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

var (
	ErrInvoiceNotRetryable             = errors.New("invoice is not retryable")
	ErrCollectionPaymentMethodRequired = errors.New("collection payment method required")
	ErrCollectionPaymentMethodInvalid  = errors.New("collection payment method invalid")
	ErrInvoiceRetryInProgress          = errors.New("invoice retry is in progress")
	ErrInvoiceRetryOutcomeUnknown      = errors.New("invoice retry outcome is unknown")
	ErrInvoiceRetryIdempotencyConflict = errors.New("invoice retry idempotency conflict")
)

type InvoiceCollectionRetryRequest struct {
	InvoiceID       uuid.UUID
	IdempotencyKey  string
	PaymentMethodID uuid.UUID
}

type InvoiceCollectionRetryResult struct {
	Invoice  *models.Invoice
	Attempt  models.InvoicePaymentAttempt
	Replayed bool
}

const (
	collectionAttemptInProgress = "collection_attempt_in_progress"
	collectionOutcomeUnknown    = "collection_outcome_unknown"
)

// ListInvoicePaymentAttempts returns one payer-owned invoice's collection
// history, newest attempt first.
func (s *MoneyService) ListInvoicePaymentAttempts(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID, limit, offset int) ([]models.InvoicePaymentAttempt, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, 0, fmt.Errorf("payer required")
	}
	if invoiceID == uuid.Nil {
		return nil, 0, fmt.Errorf("invoice_id required")
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, 0, err
	}

	// Distinguish an existing invoice with no attempts from an inaccessible ID.
	if _, err := s.GetInvoiceByID(ctx, payer, invoiceID); err != nil {
		return nil, 0, fmt.Errorf("load invoice: %w", err)
	}

	q := s.db.Gen(ctx)
	total, err := q.CountInvoicePaymentAttemptsByPayer(ctx, gen.CountInvoicePaymentAttemptsByPayerParams{
		MerchantID: tid.UUID(),
		CustomerID: payer.UUID(),
		InvoiceID:  invoiceID,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("count invoice payment attempts: %w", err)
	}
	rows, err := q.ListInvoicePaymentAttemptsByPayer(ctx, gen.ListInvoicePaymentAttemptsByPayerParams{
		MerchantID: tid.UUID(),
		CustomerID: payer.UUID(),
		InvoiceID:  invoiceID,
		Limit:      int64(limit),
		Offset:     int64(offset),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list invoice payment attempts: %w", err)
	}
	attempts := make([]models.InvoicePaymentAttempt, 0, len(rows))
	for _, row := range rows {
		attempts = append(attempts, invoicePaymentAttemptFromGen(row))
	}
	return attempts, int(total), nil
}

// SetInvoiceCollectionPaymentMethod selects the payer-owned saved method used
// for automatic invoice collection in one billing currency.
func (s *MoneyService) SetInvoiceCollectionPaymentMethod(ctx context.Context, payer identity.CustomerID, currency string, paymentMethodID uuid.UUID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	if paymentMethodID == uuid.Nil {
		return fmt.Errorf("payment_method_id required")
	}
	currency = normalizeCurrency(currency)
	if err := RequireBillingCurrency(currency); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		method, err := q.GetPaymentMethodByID(ctx, paymentMethodID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCollectionPaymentMethodInvalid
			}
			return fmt.Errorf("load collection payment method: %w", err)
		}
		if method.MerchantID != tid.UUID() || method.CustomerID != payer.UUID() {
			return ErrCollectionPaymentMethodInvalid
		}
		if strings.TrimSpace(method.ParkReason) != "" {
			return fmt.Errorf("%w: payment method is parked", ErrCollectionPaymentMethodInvalid)
		}
		descriptor, ok := rails.Lookup(models.Rail(method.Rail))
		if !ok {
			return fmt.Errorf("%w: unknown rail %q", ErrCollectionPaymentMethodInvalid, method.Rail)
		}
		if !descriptor.SupportsChargeSavedMethod {
			return fmt.Errorf("%w: rail %q does not support invoice collection", ErrCollectionPaymentMethodInvalid, method.Rail)
		}
		if err := s.ensureSettingsRowTx(ctx, q, tid.UUID(), payer.UUID(), currency, BillingModePrepaid, now); err != nil {
			return fmt.Errorf("ensure money account settings: %w", err)
		}
		n, err := q.SetMoneyAccountCollectionPaymentMethod(ctx, gen.SetMoneyAccountCollectionPaymentMethodParams{
			MerchantID:      tid.UUID(),
			CustomerID:      payer.UUID(),
			Currency:        currency,
			PaymentMethodID: &paymentMethodID,
			Now:             now,
		})
		if err != nil {
			return fmt.Errorf("set collection payment method: %w", err)
		}
		if n != 1 {
			return fmt.Errorf("set collection payment method: settings row not found")
		}
		return nil
	})
}

// RetryInvoiceCollection reopens a failed automatic-collection invoice and
// immediately attempts collection through the existing provider-safe path.
func (s *MoneyService) RetryInvoiceCollection(ctx context.Context, charger Charger, payer identity.CustomerID, invoiceID uuid.UUID) (*models.Invoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if charger == nil {
		return nil, fmt.Errorf("charger required")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if invoiceID == uuid.Nil {
		return nil, fmt.Errorf("invoice_id required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.expireStaleInvoiceCollectionRetryClaims(ctx, tid.UUID(), s.now()); err != nil {
		return nil, err
	}
	claim, ok, err := s.claimInvoiceCollection(ctx, payer, invoiceID, 0, true, s.now())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrInvoiceNotRetryable
	}
	if _, err := s.chargeClaimedInvoice(ctx, charger, claim); err != nil {
		return nil, fmt.Errorf("retry invoice collection: %w", err)
	}
	return s.GetInvoiceByID(ctx, payer, invoiceID)
}

// RetryInvoiceCollectionIdempotent binds a client retry key to one saved
// payment method. Reusing the key returns the durable terminal attempt without
// dispatching another provider charge.
func (s *MoneyService) RetryInvoiceCollectionIdempotent(ctx context.Context, charger Charger, payer identity.CustomerID, request InvoiceCollectionRetryRequest) (*InvoiceCollectionRetryResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if charger == nil {
		return nil, fmt.Errorf("charger required")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if request.InvoiceID == uuid.Nil || request.PaymentMethodID == uuid.Nil {
		return nil, fmt.Errorf("invoice_id and payment_method_id required")
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 255 {
		return nil, fmt.Errorf("idempotency_key must be between 1 and 255 bytes")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.expireStaleInvoiceCollectionRetryClaims(ctx, tid.UUID(), s.now()); err != nil {
		return nil, err
	}
	providerKey := invoiceRetryIdempotencyKey(request.InvoiceID, request.IdempotencyKey)
	attemptKey := invoiceRetryAttemptKey(providerKey, request.PaymentMethodID)
	claim, replay, claimed, err := s.claimInvoiceCollectionWithOptions(ctx, payer, request.InvoiceID, invoiceCollectionClaimOptions{
		manual: true, now: s.now(), idempotencyKey: attemptKey,
		providerIdempotencyKey: providerKey, paymentMethodID: &request.PaymentMethodID,
	})
	if err != nil {
		return nil, err
	}
	if replay != nil {
		invoice, err := s.GetInvoiceByID(ctx, payer, request.InvoiceID)
		if err != nil {
			return nil, err
		}
		return &InvoiceCollectionRetryResult{Invoice: invoice, Attempt: *replay, Replayed: true}, nil
	}
	if !claimed {
		return nil, ErrInvoiceNotRetryable
	}
	if _, err := s.chargeClaimedInvoice(ctx, charger, claim); err != nil {
		return nil, fmt.Errorf("retry invoice collection: %w", err)
	}
	loadCtx, cancel := collectionCleanupContext(ctx)
	defer cancel()
	invoice, err := s.GetInvoiceByID(loadCtx, payer, request.InvoiceID)
	if err != nil {
		return nil, err
	}
	attemptRow, err := s.db.Gen(loadCtx).GetInvoicePaymentAttemptByKey(loadCtx, gen.GetInvoicePaymentAttemptByKeyParams{
		MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceID: request.InvoiceID, IdempotencyKey: &attemptKey,
	})
	if err != nil {
		return nil, fmt.Errorf("load invoice retry outcome: %w", err)
	}
	return &InvoiceCollectionRetryResult{Invoice: invoice, Attempt: invoicePaymentAttemptFromGen(attemptRow)}, nil
}

func invoiceRetryIdempotencyKey(invoiceID uuid.UUID, clientKey string) string {
	digest := sha256.Sum256([]byte(invoiceID.String() + "\x00" + clientKey))
	return "ir_" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func invoiceRetryAttemptKey(providerKey string, paymentMethodID uuid.UUID) string {
	return providerKey + ":" + paymentMethodID.String()
}

func invoiceCollectionRetryable(invoice *models.Invoice) bool {
	return invoice != nil &&
		invoice.CollectionMethod == CollectionChargeAutomatically &&
		(invoice.Status == "past_due" || invoice.Status == "uncollectible") &&
		invoice.AmountDue > 0 &&
		derefStr(invoice.LastCollectionFailureCode) != collectionAttemptInProgress &&
		derefStr(invoice.LastCollectionFailureCode) != collectionOutcomeUnknown
}

func scheduledInvoiceCollectionEligible(invoice *models.Invoice, minThreshold int64, now time.Time) bool {
	if invoice == nil || invoice.CollectionMethod != CollectionChargeAutomatically ||
		(invoice.Status != "open" && invoice.Status != "past_due") || invoice.AmountDue <= 0 ||
		derefStr(invoice.LastCollectionFailureCode) == collectionAttemptInProgress ||
		derefStr(invoice.LastCollectionFailureCode) == collectionOutcomeUnknown ||
		(minThreshold > 0 && invoice.AmountDue < minThreshold) {
		return false
	}
	if invoice.DueAt != nil && invoice.DueAt.After(now) {
		return false
	}
	return invoice.CollectionFailureCount == 0 && invoice.NextCollectionAttemptAt == nil ||
		invoice.NextCollectionAttemptAt != nil && !invoice.NextCollectionAttemptAt.After(now)
}

type invoiceCollectionClaimOptions struct {
	minThreshold           int64
	manual                 bool
	now                    time.Time
	idempotencyKey         string
	providerIdempotencyKey string
	paymentMethodID        *uuid.UUID
}

func (s *MoneyService) claimInvoiceCollection(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID, minThreshold int64, manual bool, now time.Time) (*invoiceCollectionClaim, bool, error) {
	claim, _, claimed, err := s.claimInvoiceCollectionWithOptions(ctx, payer, invoiceID, invoiceCollectionClaimOptions{
		minThreshold: minThreshold, manual: manual, now: now,
	})
	return claim, claimed, err
}

func (s *MoneyService) claimInvoiceCollectionWithOptions(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID, options invoiceCollectionClaimOptions) (*invoiceCollectionClaim, *models.InvoicePaymentAttempt, bool, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	var claim *invoiceCollectionClaim
	var replay *models.InvoicePaymentAttempt
	claimed := false
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.GetInvoiceForPayerForUpdate(ctx, gen.GetInvoiceForPayerForUpdateParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), ID: invoiceID,
		})
		if errors.Is(err, pgx.ErrNoRows) && !options.manual {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock invoice collection: %w", err)
		}
		invoice, err := invoiceFromGen(row)
		if err != nil {
			return fmt.Errorf("map invoice collection: %w", err)
		}
		if options.idempotencyKey != "" {
			row, err := q.GetInvoicePaymentAttemptByClientKey(ctx, gen.GetInvoicePaymentAttemptByClientKeyParams{
				MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceID: invoiceID,
				ClientKey: &options.providerIdempotencyKey,
			})
			if err == nil {
				if row.IdempotencyKey == nil || *row.IdempotencyKey != options.idempotencyKey {
					return ErrInvoiceRetryIdempotencyConflict
				}
				attempt := invoicePaymentAttemptFromGen(row)
				switch attempt.Status {
				case "failed", "settled":
					replay = &attempt
					return nil
				default:
					if derefStr(invoice.LastCollectionFailureCode) == collectionOutcomeUnknown {
						return ErrInvoiceRetryOutcomeUnknown
					}
					return ErrInvoiceRetryInProgress
				}
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("load invoice retry attempt: %w", err)
			}
			switch derefStr(invoice.LastCollectionFailureCode) {
			case collectionAttemptInProgress:
				return ErrInvoiceRetryInProgress
			case collectionOutcomeUnknown:
				return ErrInvoiceRetryOutcomeUnknown
			}
		}
		eligible := scheduledInvoiceCollectionEligible(invoice, options.minThreshold, options.now)
		if options.manual {
			eligible = invoiceCollectionRetryable(invoice)
		}
		if !eligible {
			return nil
		}
		paymentMethodID := options.paymentMethodID
		if paymentMethodID != nil {
			method, err := q.GetPaymentMethodByID(ctx, *paymentMethodID)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrCollectionPaymentMethodInvalid
				}
				return fmt.Errorf("load collection payment method: %w", err)
			}
			if method.MerchantID != tid.UUID() || method.CustomerID != payer.UUID() || strings.TrimSpace(method.ParkReason) != "" {
				return ErrCollectionPaymentMethodInvalid
			}
			descriptor, ok := rails.Lookup(models.Rail(method.Rail))
			if !ok || !descriptor.SupportsChargeSavedMethod {
				return ErrCollectionPaymentMethodInvalid
			}
		} else {
			settingsRow, err := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{
				MerchantID: tid.UUID(), CustomerID: payer.UUID(), Currency: invoice.Currency,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				if options.manual {
					return ErrCollectionPaymentMethodRequired
				}
				return nil
			}
			if err != nil {
				return fmt.Errorf("load invoice collection settings: %w", err)
			}
			settings := settingsFromGen(settingsRow)
			paymentMethodID = settings.CollectionPaymentMethod
			if paymentMethodID == nil {
				paymentMethodID = settings.AutoTopupPaymentMethod
			}
			if paymentMethodID == nil {
				if options.manual {
					return ErrCollectionPaymentMethodRequired
				}
				return nil
			}
		}
		attempts, err := q.CountInvoicePaymentAttemptsByPayer(ctx, gen.CountInvoicePaymentAttemptsByPayerParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceID: invoiceID,
		})
		if err != nil {
			return fmt.Errorf("count invoice collection attempts: %w", err)
		}
		attemptID := uuidutil.NewV7()
		key := options.idempotencyKey
		if key == "" {
			key = fmt.Sprintf("invoice:%s:attempt:%d", invoiceID, attempts)
		}
		updated, err := q.SetInvoiceCollectionClaim(ctx, gen.SetInvoiceCollectionClaimParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), Now: options.now, InvoiceID: invoiceID,
		})
		if err != nil {
			return fmt.Errorf("set invoice collection claim: %w", err)
		}
		if updated != 1 {
			return nil
		}
		if err := q.InsertInvoicePayment(ctx, gen.InsertInvoicePaymentParams{
			ID: attemptID, MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceID: invoiceID,
			Currency: invoice.Currency, Amount: invoice.AmountDue, Status: "attempted",
			AttemptedAt: options.now, CreatedAt: options.now, UpdatedAt: options.now,
			PaymentMethodID: paymentMethodID, IdempotencyKey: &key,
		}); err != nil {
			return fmt.Errorf("record invoice collection claim: %w", err)
		}
		claim = &invoiceCollectionClaim{
			account: invoiceArrearsAccount{
				InvoiceID: invoice.ID, MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID,
				Currency: invoice.Currency, AmountDue: invoice.AmountDue,
				FailureCount: invoice.CollectionFailureCount, FirstFailureAt: invoice.CollectionFailedAt,
				PaymentMethodID: paymentMethodID,
			},
			previous: invoice, attemptID: attemptID, idempotencyKey: key,
		}
		if options.providerIdempotencyKey != "" {
			claim.idempotencyKey = options.providerIdempotencyKey
		}
		claimed = true
		return nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	return claim, replay, claimed, nil
}

func (s *MoneyService) markInvoiceCollectionOutcomeUnknown(ctx context.Context, claim *invoiceCollectionClaim, now time.Time) error {
	updated, err := s.db.Gen(ctx).MarkInvoiceCollectionOutcomeUnknown(ctx, gen.MarkInvoiceCollectionOutcomeUnknownParams{
		MerchantID: claim.account.MerchantID,
		CustomerID: claim.account.CustomerID,
		Now:        now,
		InvoiceID:  claim.account.InvoiceID,
	})
	if err != nil {
		return fmt.Errorf("mark invoice collection outcome unknown: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("mark invoice collection outcome unknown: invoice claim lost")
	}
	return nil
}

func (s *MoneyService) releaseInvoiceCollectionClaim(ctx context.Context, claim *invoiceCollectionClaim, now time.Time) error {
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		invoice := claim.previous
		_, err := q.ReleaseInvoiceCollectionRetry(ctx, gen.ReleaseInvoiceCollectionRetryParams{
			MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, Status: invoice.Status,
			NextAttemptAt: invoice.NextCollectionAttemptAt, FailureCode: invoice.LastCollectionFailureCode,
			FailureMessage: invoice.LastCollectionFailureMessage, UncollectibleAt: invoice.UncollectibleAt,
			Now: now, InvoiceID: invoice.ID,
		})
		if err != nil {
			return fmt.Errorf("release invoice collection claim: %w", err)
		}
		deleted, err := q.DeleteClaimedInvoicePaymentAttempt(ctx, gen.DeleteClaimedInvoicePaymentAttemptParams{
			MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID,
			InvoiceID: invoice.ID, AttemptID: claim.attemptID,
		})
		if err != nil {
			return fmt.Errorf("delete invoice collection claim: %w", err)
		}
		if deleted != 1 {
			return fmt.Errorf("delete invoice collection claim: payment attempt claim lost")
		}
		return nil
	})
}

func isCollectionOutcomeAmbiguous(err error) bool {
	return nmi.IsTransportAmbiguous(err) || basistheory.IsTransportAmbiguous(err)
}

func collectionCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}
