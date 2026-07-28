package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #828: ambiguity ⇒ VERIFY. An invoice parked `collection_outcome_unknown`
// (ambiguous transport error, or a crashed claim taken over by the stale-claim
// sweep) is resolved by READING the provider — the same posture as the intent
// verifier on the subscription side — instead of parking forever:
//
//	settled at provider   -> apply (the existing claimed-settle path; a mutated
//	                         invoice records the payment UNAPPLIED + alerts)
//	no successful sale    -> release the claim; the schedule resumes
//	read failed/unarmed   -> stays unknown; next pass retries
//	rail without a read   -> stays unknown; the admin unpark surface owns it

// ErrInvoiceNotParkedUnknown is returned by UnparkInvoiceCollection when the
// invoice is not in the collection_outcome_unknown park.
var ErrInvoiceNotParkedUnknown = errors.New("invoice collection outcome is not parked unknown")

// invoiceVerifyMinAge keeps the verifier off attempts younger than this so a
// just-sent charge has settled provider-side before the read.
const invoiceVerifyMinAge = 10 * time.Minute

const invoiceVerifyBatch = 100

// CollectionVerifyResult reports one provider read for an in-doubt charge.
type CollectionVerifyResult struct {
	// Supported = the method's rail has a provider read for in-doubt charges.
	// false leaves the invoice parked for the admin unpark surface.
	Supported bool
	// Settled = a successful sale carrying the wire order reference exists at
	// the provider (money moved, whenever that happened).
	Settled       bool
	TransactionID string
}

// CollectionVerifier answers "did the collection charge carrying this wire
// order reference settle?" by reading the provider. Implemented by the
// store-armed credential plane; faked in tests.
type CollectionVerifier interface {
	VerifyCollectionCharge(ctx context.Context, method gen.OpenrailsPaymentMethod, wireOrderRef string) (CollectionVerifyResult, error)
}

// InvoiceUnknownResolution summarizes one resolver pass.
type InvoiceUnknownResolution struct {
	Examined int
	Settled  int
	Released int
	Skipped  int
}

// ResolveUnknownInvoiceCollections resolves the merchant's parked
// collection_outcome_unknown invoices via provider reads. Per-invoice
// problems skip that invoice (it stays parked for the next pass); only
// infrastructure failure returns an error.
func (s *MoneyService) ResolveUnknownInvoiceCollections(ctx context.Context, verifier CollectionVerifier) (InvoiceUnknownResolution, error) {
	var stats InvoiceUnknownResolution
	if s == nil || s.db == nil {
		return stats, fmt.Errorf("money service not initialized")
	}
	if verifier == nil {
		return stats, fmt.Errorf("collection verifier required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return stats, err
	}
	now := s.now()
	rows, err := s.db.Gen(ctx).ListUnknownOutcomeInvoices(ctx, gen.ListUnknownOutcomeInvoicesParams{
		MerchantID:       tid.UUID(),
		ResolvableBefore: now.Add(-invoiceVerifyMinAge),
		Batch:            invoiceVerifyBatch,
	})
	if err != nil {
		return stats, fmt.Errorf("list unknown-outcome invoices: %w", err)
	}
	for _, row := range rows {
		stats.Examined++
		outcome, err := s.resolveUnknownInvoice(ctx, verifier, row)
		if err != nil {
			stats.Skipped++
			log.WithContext(ctx).WithError(err).WithField("invoice_id", row.ID).
				Warn("invoice collection verifier: unresolved this pass; invoice stays parked")
			continue
		}
		switch outcome {
		case unknownResolvedSettled:
			stats.Settled++
		case unknownResolvedReleased:
			stats.Released++
		default:
			stats.Skipped++
		}
	}
	return stats, nil
}

type unknownResolution int

const (
	unknownResolutionSkipped unknownResolution = iota
	unknownResolvedSettled
	unknownResolvedReleased
)

func (s *MoneyService) resolveUnknownInvoice(ctx context.Context, verifier CollectionVerifier, row gen.ListUnknownOutcomeInvoicesRow) (unknownResolution, error) {
	q := s.db.Gen(ctx)
	attempt, err := q.GetLatestAttemptedInvoicePayment(ctx, gen.GetLatestAttemptedInvoicePaymentParams{
		MerchantID: row.MerchantID, CustomerID: row.CustomerID, InvoiceID: row.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown park without a claimed attempt: settled-but-unapplied residue
		// (needs repair) or manual surgery — never auto-release without evidence.
		return unknownResolutionSkipped, fmt.Errorf("no claimed attempt on record; admin unpark owns this residue")
	}
	if err != nil {
		return unknownResolutionSkipped, fmt.Errorf("load claimed attempt: %w", err)
	}
	if attempt.IdempotencyKey == nil || attempt.PaymentMethodID == nil {
		return unknownResolutionSkipped, fmt.Errorf("claimed attempt %s lacks idempotency key or payment method", attempt.ID)
	}
	method, err := q.GetPaymentMethodByID(ctx, *attempt.PaymentMethodID)
	if err != nil {
		return unknownResolutionSkipped, fmt.Errorf("load payment method %s: %w", *attempt.PaymentMethodID, err)
	}

	providerKey := collectionProviderKey(*attempt.IdempotencyKey, *attempt.PaymentMethodID)
	res, err := verifier.VerifyCollectionCharge(ctx, method, nmiWireOrderRef(providerKey))
	if err != nil {
		return unknownResolutionSkipped, fmt.Errorf("provider read: %w", err)
	}
	if !res.Supported {
		return unknownResolutionSkipped, fmt.Errorf("rail %q has no provider read for in-doubt charges; admin unpark owns it", method.Rail)
	}
	now := s.now()
	if res.Settled {
		claim := &invoiceCollectionClaim{
			account: invoiceArrearsAccount{
				InvoiceID:  row.ID,
				MerchantID: row.MerchantID,
				CustomerID: row.CustomerID,
				Currency:   attempt.Currency,
				// The attempt row holds the claimed snapshot — the amount the
				// provider actually charged, NOT the invoice's current amount_due.
				AmountDue:       attempt.Amount,
				PaymentMethodID: attempt.PaymentMethodID,
			},
			attemptID:      attempt.ID,
			idempotencyKey: providerKey,
		}
		if _, err := s.settleClaimedInvoiceCharge(ctx, claim, method.Rail, res.TransactionID, "", now, true); err != nil {
			return unknownResolutionSkipped, fmt.Errorf("apply verified charge: %w", err)
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"invoice_id": row.ID, "rail_payment_id": res.TransactionID,
		}).Warn("invoice collection verifier: in-doubt charge CONFIRMED at provider; payment applied")
		return unknownResolvedSettled, nil
	}

	// Clean read, no successful sale: no money moved. Release the claim so the
	// schedule resumes (the failure count is untouched — nothing new failed).
	released := false
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		deleted, err := q.DeleteClaimedInvoicePaymentAttempt(ctx, gen.DeleteClaimedInvoicePaymentAttemptParams{
			MerchantID: row.MerchantID, CustomerID: row.CustomerID,
			InvoiceID: row.ID, AttemptID: attempt.ID,
		})
		if err != nil {
			return fmt.Errorf("delete claimed attempt: %w", err)
		}
		if deleted != 1 {
			return fmt.Errorf("claimed attempt no longer held")
		}
		n, err := q.ResolveInvoiceCollectionUnknown(ctx, gen.ResolveInvoiceCollectionUnknownParams{
			MerchantID: row.MerchantID, CustomerID: row.CustomerID, InvoiceID: row.ID,
			NextAttemptAt: unknownReleaseNextAttempt(row.CollectionFailureCount, now), Now: now,
		})
		if err != nil {
			return fmt.Errorf("clear unknown park: %w", err)
		}
		released = n == 1
		return nil
	})
	if err != nil {
		return unknownResolutionSkipped, err
	}
	if !released {
		return unknownResolutionSkipped, fmt.Errorf("unknown park no longer present")
	}
	log.WithContext(ctx).WithField("invoice_id", row.ID).
		Info("invoice collection verifier: in-doubt charge verified NOT executed; claim released, schedule resumes")
	return unknownResolvedReleased, nil
}

// UnparkInvoiceCollection is the ADMIN surface for residue verification
// cannot classify (rails without a provider read, settled-but-unapplied
// repairs): it releases a collection_outcome_unknown park by operator
// judgment, deleting any still-claimed attempt, so the schedule resumes. If
// the in-doubt charge DID settle at the provider, unparking can lead to a
// second charge — confirm provider-side first.
func (s *MoneyService) UnparkInvoiceCollection(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() || invoiceID == uuid.Nil {
		return fmt.Errorf("payer and invoice_id required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.GetInvoiceForPayerForUpdate(ctx, gen.GetInvoiceForPayerForUpdateParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), ID: invoiceID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("invoice not found")
		}
		if err != nil {
			return fmt.Errorf("lock invoice: %w", err)
		}
		if derefStr(row.LastCollectionFailureCode) != collectionOutcomeUnknown {
			return ErrInvoiceNotParkedUnknown
		}
		attempt, err := q.GetLatestAttemptedInvoicePayment(ctx, gen.GetLatestAttemptedInvoicePaymentParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceID: invoiceID,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("load claimed attempt: %w", err)
		}
		if err == nil {
			if _, err := q.DeleteClaimedInvoicePaymentAttempt(ctx, gen.DeleteClaimedInvoicePaymentAttemptParams{
				MerchantID: tid.UUID(), CustomerID: payer.UUID(),
				InvoiceID: invoiceID, AttemptID: attempt.ID,
			}); err != nil {
				return fmt.Errorf("delete claimed attempt: %w", err)
			}
		}
		n, err := q.ResolveInvoiceCollectionUnknown(ctx, gen.ResolveInvoiceCollectionUnknownParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceID: invoiceID,
			NextAttemptAt: unknownReleaseNextAttempt(row.CollectionFailureCount, now), Now: now,
		})
		if err != nil {
			return fmt.Errorf("clear unknown park: %w", err)
		}
		if n != 1 {
			return ErrInvoiceNotParkedUnknown
		}
		log.WithContext(ctx).WithField("invoice_id", invoiceID).
			Warn("invoice collection UNPARKED by operator; schedule resumes")
		return nil
	})
}

// unknownReleaseNextAttempt restores eligibility after a release: a fresh
// invoice (no recorded failures) is eligible bare, a mid-schedule one was due
// when it was claimed — due now.
func unknownReleaseNextAttempt(failureCount int32, now time.Time) *time.Time {
	if failureCount == 0 {
		return nil
	}
	return &now
}

// collectionProviderKey recovers the PROVIDER idempotency key (the wire
// order-ref input, and the ledger dedupe source id) from an attempt row's
// stored key: the #169 idempotent-retry path stores providerKey +
// ":<payment-method-uuid>"; every other path stores the provider key itself.
func collectionProviderKey(attemptKey string, paymentMethodID uuid.UUID) string {
	return strings.TrimSuffix(attemptKey, ":"+paymentMethodID.String())
}
