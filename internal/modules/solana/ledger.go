package solana

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ReferenceKind says what a landed transaction on a reference means.
type ReferenceKind string

const (
	ReferencePurchase  ReferenceKind = "purchase"
	ReferenceSubscribe ReferenceKind = "subscribe"
	ReferenceLifecycle ReferenceKind = "lifecycle"
)

const (
	// SettlementGrace bounds how long past its quote expiry a landed transfer
	// is still credited at the quoted amount (SEC-33). Later money is recorded
	// for review, never credited.
	SettlementGrace = 30 * time.Minute
	// watchWindow keeps a settled purchase reference watched, so a second or
	// late transfer to it is recorded rather than lost.
	watchWindow = 7 * 24 * time.Hour
)

// Reference statuses.
const (
	ReferencePending   = "pending"
	ReferenceConfirmed = "confirmed"
	ReferenceExpired   = "expired"
)

type Disposition string

const (
	Credited Disposition = "credited"
	Review   Disposition = "review"
	Ignored  Disposition = "ignored"
)

// Review reasons. Overpaid accompanies a credit; the rest mean the money was
// not credited and needs a refund or an operator decision.
const (
	ReasonOverpaid      = "overpaid"
	ReasonUnderpaid     = "underpaid"
	ReasonAlreadyPaid   = "already_paid"
	ReasonLate          = "late"
	ReasonSessionClosed = "session_closed"
)

var (
	// ErrSignatureClaimed: the signature is already credited or reviewed on
	// another reference. One on-chain transfer settles at most one checkout.
	ErrSignatureClaimed = errors.New("solana: signature already recorded on another reference")
	ErrUnknownReference = errors.New("solana: unknown pay reference")
)

// ObservedTransfer is one landed transaction as read from the chain.
type ObservedTransfer struct {
	Signature string
	Amount    uint64
	Payer     string
	LandedAt  *time.Time
}

// Receipt is the durable record of one signature on one reference.
type Receipt struct {
	Reference      string
	Signature      string
	SessionID      uuid.UUID
	Disposition    Disposition
	ReviewReason   string
	TokenMint      string
	ExpectedAmount uint64
	ReceivedAmount uint64
	Payer          string
	LandedAt       *time.Time
	PaymentID      *uuid.UUID
}

// Decide classifies a transfer on a purchase reference. Callers resolve the
// same signature seen before (idempotency) first.
func Decide(ref gen.OpenrailsSolanaPayReference, sessionOpen bool, expected uint64, t ObservedTransfer, now time.Time) (Disposition, string) {
	landed := now
	if t.LandedAt != nil {
		landed = *t.LandedAt
	}
	switch {
	case t.Amount == 0:
		return Ignored, ""
	case ref.Status == ReferenceConfirmed:
		return Review, ReasonAlreadyPaid
	case landed.After(ref.SettleUntil):
		return Review, ReasonLate
	case !sessionOpen:
		return Review, ReasonSessionClosed
	case t.Amount < expected:
		return Review, ReasonUnderpaid
	case t.Amount > expected:
		return Credited, ReasonOverpaid
	}
	return Credited, ""
}

// PayLedger owns the solana_pay_* rows. Bind it to a transaction to make its
// writes part of a settlement.
type PayLedger struct{ db *db.DB }

func NewPayLedger(d *db.DB) *PayLedger { return &PayLedger{db: d} }

// Register gives a checkout attempt its reference. A second call for the same
// attempt returns the existing row; a different reference is refused.
func (l *PayLedger) Register(ctx context.Context, kind ReferenceKind, sessionID uuid.UUID, reference string, quoteExpiresAt, now time.Time) (gen.OpenrailsSolanaPayReference, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.OpenrailsSolanaPayReference{}, err
	}
	settle := quoteExpiresAt.Add(SettlementGrace)
	watch := settle
	if kind == ReferencePurchase {
		watch = settle.Add(watchWindow)
	}
	row, err := l.db.Gen(ctx).RegisterSolanaPayReference(ctx, gen.RegisterSolanaPayReferenceParams{
		MerchantID: mid.UUID(), Reference: reference, CheckoutSessionID: sessionID, Kind: string(kind),
		SettleUntil: settle, WatchUntil: watch, Now: now,
	})
	if err != nil {
		return row, err
	}
	if row.Reference != reference {
		return row, fmt.Errorf("checkout session %s already has Solana Pay reference %s", sessionID, row.Reference)
	}
	return row, nil
}

func (l *PayLedger) Get(ctx context.Context, reference string) (gen.OpenrailsSolanaPayReference, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.OpenrailsSolanaPayReference{}, err
	}
	return l.db.Gen(ctx).GetSolanaPayReference(ctx, gen.GetSolanaPayReferenceParams{MerchantID: mid.UUID(), Reference: reference})
}

// Lock takes the reference row for the rest of the caller's transaction:
// every settlement of one reference is serialized behind it.
func (l *PayLedger) Lock(ctx context.Context, reference string) (gen.OpenrailsSolanaPayReference, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.OpenrailsSolanaPayReference{}, err
	}
	row, err := l.db.Gen(ctx).LockSolanaPayReference(ctx, gen.LockSolanaPayReferenceParams{MerchantID: mid.UUID(), Reference: reference})
	if db.IsNotFound(err) {
		return row, ErrUnknownReference
	}
	return row, err
}

func (l *PayLedger) Receipt(ctx context.Context, reference, signature string) (*Receipt, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := l.db.Gen(ctx).GetSolanaPayReceipt(ctx, gen.GetSolanaPayReceiptParams{MerchantID: mid.UUID(), Reference: reference, Signature: signature})
	if db.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return receiptFromRow(row), nil
}

func (l *PayLedger) KnownSignatures(ctx context.Context, reference string) (map[string]bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	sigs, err := l.db.Gen(ctx).ListSolanaPayReceiptSignatures(ctx, gen.ListSolanaPayReceiptSignaturesParams{MerchantID: mid.UUID(), Reference: reference})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(sigs))
	for _, s := range sigs {
		out[s] = true
	}
	return out, nil
}

// Confirm moves pending|expired -> confirmed with the credited signature.
func (l *PayLedger) Confirm(ctx context.Context, reference, signature string, now time.Time) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	n, err := l.db.Gen(ctx).ConfirmSolanaPayReference(ctx, gen.ConfirmSolanaPayReferenceParams{MerchantID: mid.UUID(), Reference: reference, Signature: signature, Now: now})
	return n == 1, err
}

// Expire moves pending -> expired once nothing can be credited any more.
func (l *PayLedger) Expire(ctx context.Context, reference string, now time.Time) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	n, err := l.db.Gen(ctx).ExpireSolanaPayReference(ctx, gen.ExpireSolanaPayReferenceParams{MerchantID: mid.UUID(), Reference: reference, Now: now})
	return n == 1, err
}

// StoreBuilt records the transaction offered to the wallet, replacing only the
// build the caller saw (previous nil = none).
func (l *PayLedger) StoreBuilt(ctx context.Context, reference, tx string, validHeight uint64, previous *int64, now time.Time) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	height, err := toInt64(validHeight)
	if err != nil {
		return false, err
	}
	n, err := l.db.Gen(ctx).StoreSolanaPayBuiltTransaction(ctx, gen.StoreSolanaPayBuiltTransactionParams{
		MerchantID: mid.UUID(), Reference: reference, BuiltTransaction: tx, BuiltValidHeight: height, PreviousValidHeight: previous, Now: now,
	})
	return n == 1, err
}

func (l *PayLedger) Schedule(ctx context.Context, reference string, at time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return l.db.Gen(ctx).ScheduleSolanaPayPoll(ctx, gen.ScheduleSolanaPayPollParams{MerchantID: mid.UUID(), Reference: reference, NextPollAt: at})
}

// Record writes a receipt. ErrSignatureClaimed when the signature is already
// credited or reviewed elsewhere; the caller's transaction must roll back.
func (l *PayLedger) Record(ctx context.Context, r Receipt, now time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	expected, err := toInt64(r.ExpectedAmount)
	if err != nil {
		return err
	}
	received, err := toInt64(r.ReceivedAmount)
	if err != nil {
		return err
	}
	n, err := l.db.Gen(ctx).InsertSolanaPayReceipt(ctx, gen.InsertSolanaPayReceiptParams{
		MerchantID: mid.UUID(), Reference: r.Reference, Signature: r.Signature, CheckoutSessionID: r.SessionID,
		Disposition: string(r.Disposition), ReviewReason: optional(r.ReviewReason), TokenMint: r.TokenMint,
		ExpectedAmount: expected, ReceivedAmount: received, Payer: optional(r.Payer), LandedAt: r.LandedAt,
		PaymentID: r.PaymentID, Now: now,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrSignatureClaimed
	}
	return nil
}

// RaiseReview surfaces money that needs a refund or an operator decision.
// Idempotent per signature.
func (l *PayLedger) RaiseReview(ctx context.Context, r Receipt, customerID string, now time.Time) error {
	if r.ReviewReason == "" {
		return nil
	}
	meta := map[string]any{
		"reference":           r.Reference,
		"checkout_session_id": r.SessionID.String(),
		"review_reason":       r.ReviewReason,
		"token_mint":          r.TokenMint,
		"expected_amount":     strconv.FormatUint(r.ExpectedAmount, 10),
		"received_amount":     strconv.FormatUint(r.ReceivedAmount, 10),
		"payer":               r.Payer,
	}
	if r.LandedAt != nil {
		meta["landed_at"] = r.LandedAt.UTC().Format(time.RFC3339)
	}
	return webhooks.RecordLedgerRepairAlert(ctx, nil, l.db, now, webhooks.LedgerRepairAlert{
		Provider:          string(models.RailSolana),
		Operation:         "solana_pay_" + r.ReviewReason,
		TransactionID:     r.Signature,
		UserID:            customerID,
		IdempotencyKey:    "solana-pay-review:" + r.Signature,
		OriginalPaymentID: r.PaymentID,
		Err:               fmt.Errorf("solana transfer %s: %s (expected %d, received %d)", r.Signature, r.ReviewReason, r.ExpectedAmount, r.ReceivedAmount),
		Metadata:          meta,
	})
}

// ClaimDue hands this replica the references due for a chain read, across
// merchants, leasing each until leaseUntil.
func ClaimDue(ctx context.Context, d *db.DB, now, leaseUntil time.Time, batch int32) ([]gen.OpenrailsSolanaPayReference, error) {
	return d.GenDirectory().ClaimDueSolanaPayReferences(ctx, gen.ClaimDueSolanaPayReferencesParams{Now: now, LeaseUntil: leaseUntil, Batch: batch})
}

// DeleteSettled removes up to batch settled references past their watch window.
func DeleteSettled(ctx context.Context, d *db.DB, now time.Time, batch int32) (int64, error) {
	return d.GenDirectory().DeleteSettledSolanaPayReferences(ctx, gen.DeleteSettledSolanaPayReferencesParams{Now: now, Batch: batch})
}

func receiptFromRow(r gen.OpenrailsSolanaPayReceipt) *Receipt {
	out := &Receipt{
		Reference: r.Reference, Signature: r.Signature, SessionID: r.CheckoutSessionID,
		Disposition: Disposition(r.Disposition), TokenMint: r.TokenMint,
		ExpectedAmount: uint64(max(r.ExpectedAmount, 0)), ReceivedAmount: uint64(max(r.ReceivedAmount, 0)),
		LandedAt: r.LandedAt, PaymentID: r.PaymentID,
	}
	if r.ReviewReason != nil {
		out.ReviewReason = *r.ReviewReason
	}
	if r.Payer != nil {
		out.Payer = *r.Payer
	}
	return out
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func toInt64(v uint64) (int64, error) {
	if v > math.MaxInt64 {
		return 0, fmt.Errorf("solana amount %d exceeds the ledger range", v)
	}
	return int64(v), nil
}
