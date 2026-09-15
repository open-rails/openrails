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
	"github.com/open-rails/openrails/pkg/merchant"
)

// #828: ambiguity ⇒ VERIFY. An invoice parked `collection_outcome_unknown`
// (ambiguous transport error, or a crashed claim taken over by the stale-claim
// sweep) is resolved by READING the provider — the same posture as the intent
// verifier on the subscription side — instead of parking forever:
//
//	settled at provider   -> apply (the existing claimed-settle path; a mutated
//	                         invoice records the payment UNAPPLIED + alerts)
//	no successful sale    -> remains unknown; a negative search is inconclusive
//	read failed/unarmed   -> stays unknown; next pass retries
//	rail without a read   -> stays unknown; exact receipt/reconciliation required

// invoiceVerifyMinAge keeps the verifier off attempts younger than this so a
// just-sent charge has settled provider-side before the read.
const invoiceVerifyMinAge = 10 * time.Minute

const invoiceVerifyBatch = 100

// CollectionVerifyResult reports one provider read for an in-doubt charge.
type CollectionVerifyResult struct {
	// Supported = the method's rail has a provider read for in-doubt charges.
	// false leaves the invoice parked for receipt reconciliation.
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
)

func (s *MoneyService) resolveUnknownInvoice(ctx context.Context, verifier CollectionVerifier, row gen.ListUnknownOutcomeInvoicesRow) (unknownResolution, error) {
	q := s.db.Gen(ctx)
	attempt, err := q.GetLatestAttemptedInvoicePayment(ctx, gen.GetLatestAttemptedInvoicePaymentParams{
		MerchantID: row.MerchantID, CustomerID: row.CustomerID, InvoiceID: row.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown park without a claimed attempt: settled-but-unapplied residue
		// (needs repair) or manual surgery — never auto-release without evidence.
		return unknownResolutionSkipped, fmt.Errorf("no claimed attempt on record; receipt reconciliation required")
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
		return unknownResolutionSkipped, fmt.Errorf("rail %q has no provider read for in-doubt charges; receipt reconciliation required", method.Rail)
	}
	now := s.now()
	if res.Settled {
		if strings.TrimSpace(res.TransactionID) == "" {
			return unknownResolutionSkipped, fmt.Errorf("positive provider result has no transaction receipt")
		}
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

	// A successful search with no match does not establish terminal
	// non-execution. Preserve the original attempt, amount and unknown park.
	return unknownResolutionSkipped, nil
}

// collectionProviderKey recovers the PROVIDER idempotency key (the wire
// order-ref input, and the ledger dedupe source id) from an attempt row's
// stored key: the #169 idempotent-retry path stores providerKey +
// ":<payment-method-uuid>"; every other path stores the provider key itself.
func collectionProviderKey(attemptKey string, paymentMethodID uuid.UUID) string {
	return strings.TrimSuffix(attemptKey, ":"+paymentMethodID.String())
}
