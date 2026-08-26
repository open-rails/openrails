package money

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	operationAuthorizationCurrency          = "USD"
	operationAuthorizationMaxIDBytes        = 255
	operationAuthorizationMaxPrincipalBytes = 255
	operationAuthorizationMaxReferenceBytes = 1024
	operationAuthorizationMaxBodyBytes      = 64 << 10
)

type OperationAuthorizationState string

const (
	OperationAuthorizationOpen     OperationAuthorizationState = "open"
	OperationAuthorizationReleased OperationAuthorizationState = "released"
	// OperationAuthorizationSettled is a schema terminal state reserved for a
	// future tx-native capture that excludes its own open reservation. This slice
	// intentionally exposes no settlement transition.
	OperationAuthorizationSettled OperationAuthorizationState = "settled"
)

var (
	ErrOperationAuthorizationConflict = errors.New("operation_authorization_conflict")
	ErrOperationAuthorizationNotFound = errors.New("operation_authorization_not_found")
	ErrOperationAuthorizationNotOpen  = errors.New("operation_authorization_not_open")
)

// OperationAuthorizationConflict means an operation id already committed with
// a different immutable field. The field name is safe to report; body contents
// are intentionally omitted from the error.
type OperationAuthorizationConflict struct {
	Field string
}

func (e *OperationAuthorizationConflict) Error() string {
	return fmt.Sprintf("operation authorization id reused with changed %s", e.Field)
}

func (e *OperationAuthorizationConflict) Unwrap() error {
	return ErrOperationAuthorizationConflict
}

type OperationAuthorizationInput struct {
	OperationID             string
	Payer                   identity.CustomerID
	RecordOwner             string
	AuthorizedUSDMicros     int64
	ClaimReference          string
	AuthorizationBody       []byte
	AuthorizationBodySHA256 [sha256.Size]byte
}

type OperationAuthorization struct {
	OperationID             string
	MerchantID              uuid.UUID
	Payer                   identity.CustomerID
	RecordOwner             string
	LedgerAccountID         uuid.UUID
	AuthorizedUSDMicros     int64
	ClaimReference          string
	AuthorizationBody       []byte
	AuthorizationBodySHA256 [sha256.Size]byte
	State                   OperationAuthorizationState
	TerminalReference       string
	CreatedAt               time.Time
	ReleasedAt              *time.Time
	SettledAt               *time.Time
	Replayed                bool
}

// AdmissionHeldReader reads the live Redis request-admission reservation total
// while the caller holds OpenRails' PostgreSQL customer-row money lock. New
// admissions take the same lock before mutating Redis, so this snapshot cannot
// race a new hold into existence.
type AdmissionHeldReader func(context.Context) (int64, error)

// OpenOperationAuthorizationInTx validates account capacity and inserts (or
// byte-for-byte replays) an open authorization through the transaction-scoped
// DB returned by db.BindMerchantTx. It never commits or rolls back the caller's
// transaction.
func (s *MoneyService) OpenOperationAuthorizationInTx(ctx context.Context, txDB *db.DB, in OperationAuthorizationInput, readAdmissionHeld AdmissionHeldReader) (*OperationAuthorization, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if txDB == nil {
		return nil, fmt.Errorf("operation authorization requires a bound transaction")
	}
	if readAdmissionHeld == nil {
		return nil, fmt.Errorf("operation authorization requires live admission-held capacity")
	}
	if err := validateOperationAuthorizationInput(in); err != nil {
		return nil, err
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	txSvc := &MoneyService{db: txDB, clock: s.clock}
	q := txDB.Gen(ctx)

	// The customer row is the existing money serialization point. It prevents
	// two reservations for one payer from both observing the same capacity.
	bal, err := txSvc.lockBalance(ctx, q, in.Payer, in.Payer.UUID().String(), operationAuthorizationCurrency)
	if err != nil {
		return nil, err
	}

	if existing, getErr := q.GetOperationAuthorization(ctx, gen.GetOperationAuthorizationParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID,
	}); getErr == nil {
		return replayOperationAuthorization(existing, in)
	} else if !errors.Is(getErr, pgx.ErrNoRows) {
		return nil, getErr
	}

	ledgerAccountID, found, err := ledger.New(q, merchantID.UUID()).CustomerBalanceAccountID(
		ctx, in.Payer.UUID(), operationAuthorizationCurrency,
	)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("operation authorization: customer balance ledger account was not materialized")
	}

	// PG -> Redis is the sole cross-store ordering. Live admission takes this
	// same customer lock across its capacity read and Redis Lua reservation;
	// while we hold it, no new request hold can appear between this read and the
	// durable authorization insert. Capture/release may only reduce Redis held.
	admissionHeld, err := readAdmissionHeld(ctx)
	if err != nil {
		return nil, fmt.Errorf("read live admission-held capacity: %w", err)
	}
	capacity, err := subtractOperationCapacity(bal.Balance, bal.HeldBalance, "durable authorization holds")
	if err != nil {
		return nil, err
	}
	capacity, err = subtractOperationCapacity(capacity, admissionHeld, "redis admission holds")
	if err != nil {
		return nil, err
	}
	settings, err := txSvc.getAccountSettings(ctx, in.Payer, operationAuthorizationCurrency)
	if err != nil {
		return nil, err
	}
	if settings.BillingMode == BillingModeArrears {
		outstanding, oerr := txSvc.moneyLedger(q, merchantID.UUID()).OutstandingOwed(
			ctx, in.Payer.UUID(), operationAuthorizationCurrency,
		)
		if oerr != nil {
			return nil, oerr
		}
		if settings.CreditLimitAmount < 0 {
			return nil, fmt.Errorf("operation authorization: credit limit is negative")
		}
		if outstanding < 0 {
			return nil, fmt.Errorf("operation authorization: outstanding owed is negative")
		}
		if settings.CreditLimitAmount > outstanding {
			remainingCredit := settings.CreditLimitAmount - outstanding
			capacity, err = addOperationCapacity(capacity, remainingCredit)
			if err != nil {
				return nil, err
			}
		}
	}
	if capacity < in.AuthorizedUSDMicros {
		return nil, ErrInsufficientCredits
	}

	row, err := q.InsertOperationAuthorization(ctx, gen.InsertOperationAuthorizationParams{
		OperationID:             in.OperationID,
		MerchantID:              merchantID.UUID(),
		PayerID:                 in.Payer.UUID(),
		RecordOwner:             in.RecordOwner,
		LedgerAccountID:         ledgerAccountID,
		AuthorizedUsdMicros:     in.AuthorizedUSDMicros,
		ClaimReference:          in.ClaimReference,
		AuthorizationBodyBytes:  in.AuthorizationBody,
		AuthorizationBodyDigest: in.AuthorizationBodySHA256[:],
	})
	if err == nil {
		return operationAuthorizationFromRow(row, false), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// A different payer under this merchant can race on the same operation id
	// because it takes a different customer lock. The merchant-scoped unique key
	// serializes that identity; resolve the lost insert as replay or exact conflict.
	existing, err := q.GetOperationAuthorization(ctx, gen.GetOperationAuthorizationParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("operation authorization conflict row is not visible in its merchant scope")
	}
	if err != nil {
		return nil, err
	}
	return replayOperationAuthorization(existing, in)
}

func subtractOperationCapacity(capacity, held int64, source string) (int64, error) {
	if held < 0 {
		return 0, fmt.Errorf("operation authorization: %s is negative", source)
	}
	if capacity < math.MinInt64+held {
		return 0, fmt.Errorf("operation authorization: capacity underflow subtracting %s", source)
	}
	return capacity - held, nil
}

func addOperationCapacity(capacity, addition int64) (int64, error) {
	if addition < 0 {
		return 0, fmt.Errorf("operation authorization: capacity addition is negative")
	}
	if capacity > math.MaxInt64-addition {
		return 0, fmt.Errorf("operation authorization: capacity overflow adding remaining credit")
	}
	return capacity + addition, nil
}

func validateOperationAuthorizationInput(in OperationAuthorizationInput) error {
	if err := validateOperationAuthorizationText("operation_id", in.OperationID, operationAuthorizationMaxIDBytes); err != nil {
		return err
	}
	if in.Payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	if err := validateOperationAuthorizationText("record_owner", in.RecordOwner, operationAuthorizationMaxPrincipalBytes); err != nil {
		return err
	}
	if in.AuthorizedUSDMicros <= 0 {
		return fmt.Errorf("authorized_usd_micros must be positive")
	}
	if err := validateOperationAuthorizationText("claim_reference", in.ClaimReference, operationAuthorizationMaxReferenceBytes); err != nil {
		return err
	}
	if len(in.AuthorizationBody) == 0 {
		return fmt.Errorf("authorization_body required")
	}
	if len(in.AuthorizationBody) > operationAuthorizationMaxBodyBytes {
		return fmt.Errorf("authorization_body exceeds %d bytes", operationAuthorizationMaxBodyBytes)
	}
	if got := sha256.Sum256(in.AuthorizationBody); got != in.AuthorizationBodySHA256 {
		return fmt.Errorf("authorization_body_sha256 does not match authorization_body")
	}
	return nil
}

func validateOperationAuthorizationText(field, value string, maxBytes int) error {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s required in canonical form", field)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", field, maxBytes)
	}
	return nil
}

func replayOperationAuthorization(row gen.OpenrailsOperationAuthorization, in OperationAuthorizationInput) (*OperationAuthorization, error) {
	checks := []struct {
		field string
		same  bool
	}{
		{"payer", row.PayerID == in.Payer.UUID()},
		{"record_owner", row.RecordOwner == in.RecordOwner},
		{"authorized_usd_micros", row.AuthorizedUsdMicros == in.AuthorizedUSDMicros},
		{"claim_reference", row.ClaimReference == in.ClaimReference},
		{"authorization_body", bytes.Equal(row.AuthorizationBodyBytes, in.AuthorizationBody)},
		{"authorization_body_sha256", bytes.Equal(row.AuthorizationBodyDigest, in.AuthorizationBodySHA256[:])},
	}
	for _, check := range checks {
		if !check.same {
			return nil, &OperationAuthorizationConflict{Field: check.field}
		}
	}
	return operationAuthorizationFromRow(row, true), nil
}

// GetOperationAuthorization reads one merchant-scoped authorization. Missing
// rows return (nil, nil), matching other engine-native read primitives.
func (s *MoneyService) GetOperationAuthorization(ctx context.Context, operationID string) (*OperationAuthorization, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if err := validateOperationAuthorizationText("operation_id", operationID, operationAuthorizationMaxIDBytes); err != nil {
		return nil, err
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out *OperationAuthorization
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		row, getErr := s.db.Gen(ctx).GetOperationAuthorization(ctx, gen.GetOperationAuthorizationParams{
			MerchantID: merchantID.UUID(), OperationID: operationID,
		})
		if errors.Is(getErr, pgx.ErrNoRows) {
			return nil
		}
		if getErr != nil {
			return getErr
		}
		out = operationAuthorizationFromRow(row, false)
		return nil
	})
	return out, err
}

// ReleaseOperationAuthorization terminally releases an open reservation after
// the embedding host has proven the provider create did not happen. OpenRails
// binds the caller's opaque proof reference but owns no provider ambiguity
// logic. Repeating the same release is an idempotent replay.
func (s *MoneyService) ReleaseOperationAuthorization(ctx context.Context, operationID, releaseReference string) (*OperationAuthorization, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if err := validateOperationAuthorizationText("operation_id", operationID, operationAuthorizationMaxIDBytes); err != nil {
		return nil, err
	}
	if err := validateOperationAuthorizationText("release_reference", releaseReference, operationAuthorizationMaxReferenceBytes); err != nil {
		return nil, err
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out *OperationAuthorization
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txSvc := &MoneyService{db: s.db.NewWithPgxTx(tx), clock: s.clock}
		q := gen.New(tx)
		row, getErr := q.GetOperationAuthorization(ctx, gen.GetOperationAuthorizationParams{
			MerchantID: merchantID.UUID(), OperationID: operationID,
		})
		if errors.Is(getErr, pgx.ErrNoRows) {
			return ErrOperationAuthorizationNotFound
		}
		if getErr != nil {
			return getErr
		}
		payer := identity.CustomerID(row.PayerID)
		if _, lockErr := txSvc.lockBalance(ctx, q, payer, payer.UUID().String(), operationAuthorizationCurrency); lockErr != nil {
			return lockErr
		}
		row, getErr = q.GetOperationAuthorization(ctx, gen.GetOperationAuthorizationParams{
			MerchantID: merchantID.UUID(), OperationID: operationID,
		})
		if getErr != nil {
			return getErr
		}
		switch OperationAuthorizationState(row.State) {
		case OperationAuthorizationReleased:
			if row.TerminalReference == nil || *row.TerminalReference != releaseReference {
				return &OperationAuthorizationConflict{Field: "release_reference"}
			}
			out = operationAuthorizationFromRow(row, true)
			return nil
		case OperationAuthorizationSettled:
			return ErrOperationAuthorizationNotOpen
		case OperationAuthorizationOpen:
		default:
			return fmt.Errorf("operation authorization has invalid state %q", row.State)
		}

		released, releaseErr := q.ReleaseOperationAuthorization(ctx, gen.ReleaseOperationAuthorizationParams{
			MerchantID:        merchantID.UUID(),
			OperationID:       operationID,
			TerminalReference: releaseReference,
			ReleasedAt:        s.now().UTC(),
		})
		if releaseErr != nil {
			return releaseErr
		}
		out = operationAuthorizationFromRow(released, false)
		return nil
	})
	return out, err
}

func operationAuthorizationFromRow(row gen.OpenrailsOperationAuthorization, replayed bool) *OperationAuthorization {
	var digest [sha256.Size]byte
	copy(digest[:], row.AuthorizationBodyDigest)
	terminalReference := ""
	if row.TerminalReference != nil {
		terminalReference = *row.TerminalReference
	}
	return &OperationAuthorization{
		OperationID:             row.OperationID,
		MerchantID:              row.MerchantID,
		Payer:                   identity.CustomerID(row.PayerID),
		RecordOwner:             row.RecordOwner,
		LedgerAccountID:         row.LedgerAccountID,
		AuthorizedUSDMicros:     row.AuthorizedUsdMicros,
		ClaimReference:          row.ClaimReference,
		AuthorizationBody:       bytes.Clone(row.AuthorizationBodyBytes),
		AuthorizationBodySHA256: digest,
		State:                   OperationAuthorizationState(row.State),
		TerminalReference:       terminalReference,
		CreatedAt:               row.CreatedAt,
		ReleasedAt:              row.ReleasedAt,
		SettledAt:               row.SettledAt,
		Replayed:                replayed,
	}
}
