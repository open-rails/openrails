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
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	operationAuthorizationCurrency          = "USD"
	operationAuthorizationPassThroughSource = "operation_authorization_pass_through_provider_cost"
	operationAuthorizationMaxIDBytes        = 255
	operationAuthorizationMaxPrincipalBytes = 255
	operationAuthorizationMaxReferenceBytes = 1024
	operationAuthorizationMaxBodyBytes      = 64 << 10
)

type OperationAuthorizationState string

const (
	OperationAuthorizationOpen     OperationAuthorizationState = "open"
	OperationAuthorizationReleased OperationAuthorizationState = "released"
	// OperationAuthorizationSettled is terminal after the permanent
	// pass-through-provider-cost settlement contract runs.
	OperationAuthorizationSettled OperationAuthorizationState = "settled"
)

var (
	ErrOperationAuthorizationConflict           = openrails.ErrOperationAuthorizationConflict
	ErrOperationAuthorizationNotFound           = openrails.ErrOperationAuthorizationNotFound
	ErrOperationAuthorizationNotOpen            = openrails.ErrOperationAuthorizationNotOpen
	ErrOperationAuthorizationHasBillingEvidence = openrails.ErrOperationAuthorizationHasBillingEvidence
)

// OperationAuthorizationConflict means an operation id already committed with
// a different immutable field. The field name is safe to report; body contents
// are intentionally omitted from the error.
type OperationAuthorizationConflict = openrails.OperationAuthorizationConflict

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
	OperationID                     string
	MerchantID                      uuid.UUID
	Payer                           identity.CustomerID
	RecordOwner                     string
	LedgerAccountID                 uuid.UUID
	AuthorizedUSDMicros             int64
	ClaimReference                  string
	AuthorizationBody               []byte
	AuthorizationBodySHA256         [sha256.Size]byte
	State                           OperationAuthorizationState
	TerminalReference               string
	SettlementProviderCostUSDMicros *int64
	SettlementRatedUSDMicros        *int64
	SettlementBody                  []byte
	SettlementBodySHA256            [sha256.Size]byte
	CreatedAt                       time.Time
	ReleasedAt                      *time.Time
	SettledAt                       *time.Time
	Replayed                        bool
}

type passThroughProviderCostSettlementInput struct {
	OperationID           string
	ProviderCostUSDMicros int64
	SettlementBody        []byte
}

// OpenOperationAuthorizationInTx validates account capacity and inserts (or
// byte-for-byte replays) an open authorization through the transaction-scoped
// DB returned by db.BindMerchantTx. It never commits or rolls back the caller's
// transaction.
func (s *MoneyService) OpenOperationAuthorizationInTx(ctx context.Context, txDB *db.DB, in OperationAuthorizationInput) (*OperationAuthorization, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if txDB == nil {
		return nil, fmt.Errorf("operation authorization requires a bound transaction")
	}
	if err := validateOperationAuthorizationInput(in); err != nil {
		return nil, fmt.Errorf("%w: %v", openrails.ErrInvalid, err)
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

	capacity, err := subtractOperationCapacity(bal.Balance, bal.HeldBalance, "financial holds")
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
	if err := validateOperationID(in.OperationID); err != nil {
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

// validateOperationAuthorizationText requires text that survives JSON and
// PostgreSQL unchanged, so embedded and HTTP callers name the same row.
func validateOperationAuthorizationText(field, value string, maxBytes int) error {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s required in canonical form", field)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", field, maxBytes)
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s must be valid UTF-8 without NUL", field)
	}
	return nil
}

// validateOperationID also refuses dot segments, which no HTTP route can carry.
func validateOperationID(operationID string) error {
	if operationID == "." || operationID == ".." {
		return fmt.Errorf("operation_id %q is not a valid operation id", operationID)
	}
	return validateOperationAuthorizationText("operation_id", operationID, operationAuthorizationMaxIDBytes)
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

// settlePassThroughProviderCostInTx applies OpenRails' permanent pass-through
// provider-cost rating and performs the final customer settlement through the
// existing double-entry ledger in a caller-owned transaction. It never calls
// request admission and never commits or rolls back the transaction. It is
// unexported: only the provider-billing qualifier may supply its cost.
//
// The payer row is the money mutex. Terminally settling before the ledger helper
// excludes this authorization's full hold while every other open authorization
// stays held. The pre-authorized helper never re-gates: the rated settlement
// above the authorization is posted as owed/overdraft truth rather than clamped.
func (s *MoneyService) settlePassThroughProviderCostInTx(ctx context.Context, txDB *db.DB, in passThroughProviderCostSettlementInput) (*OperationAuthorization, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if txDB == nil {
		return nil, fmt.Errorf("operation authorization settlement requires a bound transaction")
	}
	if err := validatePassThroughProviderCostSettlementInput(in); err != nil {
		return nil, err
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	txSvc := &MoneyService{db: txDB, clock: s.clock}
	q := txDB.Gen(ctx)
	row, err := q.GetOperationAuthorization(ctx, gen.GetOperationAuthorizationParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOperationAuthorizationNotFound
	}
	if err != nil {
		return nil, err
	}
	payer := identity.CustomerID(row.PayerID)
	if _, err := txSvc.lockBalance(ctx, q, payer, row.RecordOwner, operationAuthorizationCurrency); err != nil {
		return nil, err
	}
	row, err = q.GetOperationAuthorization(ctx, gen.GetOperationAuthorizationParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID,
	})
	if err != nil {
		return nil, err
	}
	switch OperationAuthorizationState(row.State) {
	case OperationAuthorizationSettled:
		return replayPassThroughProviderCostSettlement(row, in)
	case OperationAuthorizationReleased:
		return nil, ErrOperationAuthorizationNotOpen
	case OperationAuthorizationOpen:
	default:
		return nil, fmt.Errorf("operation authorization has invalid state %q", row.State)
	}

	key := passThroughProviderCostSettlementKey(in.OperationID)
	ratedUSDMicros := in.ProviderCostUSDMicros
	if ratedUSDMicros > 0 {
		committed, err := key.requireSameAmount(ctx, q, merchantID.UUID(), payer.UUID(), operationAuthorizationCurrency, ratedUSDMicros)
		if err != nil {
			return nil, err
		}
		if committed {
			return nil, fmt.Errorf("operation authorization settlement: ledger coordinate exists without settled authorization evidence")
		}
	}
	settlementDigest := sha256.Sum256(in.SettlementBody)
	terminalReference := fmt.Sprintf("sha256:%x", settlementDigest[:])
	settled, err := q.SettleOperationAuthorizationPassThroughProviderCost(ctx, gen.SettleOperationAuthorizationPassThroughProviderCostParams{
		SettlementProviderCostUsdMicros: in.ProviderCostUSDMicros,
		SettlementRatedUsdMicros:        ratedUSDMicros,
		SettlementBodyBytes:             in.SettlementBody,
		SettlementBodyDigest:            settlementDigest[:],
		TerminalReference:               terminalReference,
		SettledAt:                       s.now().UTC(),
		MerchantID:                      merchantID.UUID(),
		OperationID:                     in.OperationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOperationAuthorizationNotOpen
	}
	if err != nil {
		return nil, err
	}

	if ratedUSDMicros > 0 {
		_, _, applied, err := txSvc.spendBalanceThenOwedTx(
			ctx, q, payer, row.RecordOwner, operationAuthorizationCurrency, key, ratedUSDMicros, true,
		)
		if err != nil {
			return nil, err
		}
		if !applied {
			return nil, fmt.Errorf("operation authorization settlement: ledger capture was not applied")
		}
	}
	return operationAuthorizationFromRow(settled, false), nil
}

func passThroughProviderCostSettlementKey(operationID string) IdempotencyKey {
	digest := sha256.Sum256([]byte(operationID))
	return MustIdempotencyKey(OpCapture, operationAuthorizationPassThroughSource, fmt.Sprintf("%x", digest[:]))
}

func validatePassThroughProviderCostSettlementInput(in passThroughProviderCostSettlementInput) error {
	if err := validateOperationID(in.OperationID); err != nil {
		return err
	}
	if in.ProviderCostUSDMicros < 0 {
		return fmt.Errorf("provider_cost_usd_micros must be nonnegative")
	}
	if len(in.SettlementBody) == 0 {
		return fmt.Errorf("settlement_body required")
	}
	if len(in.SettlementBody) > operationAuthorizationMaxBodyBytes {
		return fmt.Errorf("settlement_body exceeds %d bytes", operationAuthorizationMaxBodyBytes)
	}
	return nil
}

func replayPassThroughProviderCostSettlement(row gen.OpenrailsOperationAuthorization, in passThroughProviderCostSettlementInput) (*OperationAuthorization, error) {
	if row.SettlementProviderCostUsdMicros == nil || row.SettlementRatedUsdMicros == nil {
		return nil, fmt.Errorf("settled operation authorization has incomplete settlement amounts")
	}
	if *row.SettlementProviderCostUsdMicros != *row.SettlementRatedUsdMicros {
		return nil, fmt.Errorf("settled operation authorization violates pass-through provider-cost rating")
	}
	digest := sha256.Sum256(row.SettlementBodyBytes)
	terminalReference := fmt.Sprintf("sha256:%x", digest[:])
	if !bytes.Equal(row.SettlementBodyDigest, digest[:]) || row.TerminalReference == nil || *row.TerminalReference != terminalReference {
		return nil, fmt.Errorf("settled operation authorization has invalid derived settlement evidence")
	}
	checks := []struct {
		field string
		same  bool
	}{
		{"provider_cost_usd_micros", *row.SettlementProviderCostUsdMicros == in.ProviderCostUSDMicros},
		{"settlement_body", bytes.Equal(row.SettlementBodyBytes, in.SettlementBody)},
	}
	for _, check := range checks {
		if !check.same {
			return nil, &OperationAuthorizationConflict{Field: check.field}
		}
	}
	return operationAuthorizationFromRow(row, true), nil
}

// GetOperationAuthorization reads one merchant-scoped authorization.
func (s *MoneyService) GetOperationAuthorization(ctx context.Context, operationID string) (*OperationAuthorization, error) {
	var out *OperationAuthorization
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var err error
		out, err = s.GetOperationAuthorizationInTx(ctx, s.db, operationID)
		return err
	})
	return out, err
}

// GetOperationAuthorizationInTx reads through a caller-owned transaction, so it
// observes that transaction's uncommitted open, release, or settlement.
func (s *MoneyService) GetOperationAuthorizationInTx(ctx context.Context, txDB *db.DB, operationID string) (*OperationAuthorization, error) {
	if txDB == nil {
		return nil, fmt.Errorf("operation authorization requires a bound transaction")
	}
	if err := validateOperationID(operationID); err != nil {
		return nil, fmt.Errorf("%w: %v", openrails.ErrInvalid, err)
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := txDB.Gen(ctx).GetOperationAuthorization(ctx, gen.GetOperationAuthorizationParams{
		MerchantID: merchantID.UUID(), OperationID: operationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOperationAuthorizationNotFound
	}
	if err != nil {
		return nil, err
	}
	return operationAuthorizationFromRow(row, false), nil
}

func (s *MoneyService) ReleaseOperationAuthorization(ctx context.Context, operationID, releaseReference string) (*OperationAuthorization, error) {
	var out *OperationAuthorization
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.ReleaseOperationAuthorizationInTx(ctx, s.db.NewWithPgxTx(tx), operationID, releaseReference)
		return err
	})
	return out, err
}

// ReleaseOperationAuthorizationInTx terminally releases an open reservation in
// a caller-owned transaction, so the host's proven provider non-creation and
// the monetary release commit or roll back together. OpenRails binds the opaque
// proof reference but owns no provider ambiguity logic. Any durable billing
// qualification refuses release; repeating the same release replays.
func (s *MoneyService) ReleaseOperationAuthorizationInTx(ctx context.Context, txDB *db.DB, operationID, releaseReference string) (*OperationAuthorization, error) {
	if txDB == nil {
		return nil, fmt.Errorf("operation authorization requires a bound transaction")
	}
	if err := validateOperationID(operationID); err != nil {
		return nil, fmt.Errorf("%w: %v", openrails.ErrInvalid, err)
	}
	if err := validateOperationAuthorizationText("release_reference", releaseReference, operationAuthorizationMaxReferenceBytes); err != nil {
		return nil, fmt.Errorf("%w: %v", openrails.ErrInvalid, err)
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	q := txDB.Gen(ctx)
	params := gen.GetOperationAuthorizationParams{MerchantID: merchantID.UUID(), OperationID: operationID}
	row, err := q.GetOperationAuthorization(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOperationAuthorizationNotFound
	}
	if err != nil {
		return nil, err
	}
	// The payer row is the money mutex shared with open and settlement; state
	// is re-read under it.
	payer := identity.CustomerID(row.PayerID)
	txSvc := &MoneyService{db: txDB, clock: s.clock}
	if _, err := txSvc.lockBalance(ctx, q, payer, row.RecordOwner, operationAuthorizationCurrency); err != nil {
		return nil, err
	}
	if row, err = q.GetOperationAuthorization(ctx, params); err != nil {
		return nil, err
	}
	_, err = q.GetProviderBillingQualificationForUpdate(ctx, gen.GetProviderBillingQualificationForUpdateParams{
		MerchantID: merchantID.UUID(), OperationID: operationID,
	})
	if err == nil {
		return nil, ErrOperationAuthorizationHasBillingEvidence
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	switch OperationAuthorizationState(row.State) {
	case OperationAuthorizationReleased:
		if row.TerminalReference == nil || *row.TerminalReference != releaseReference {
			return nil, &OperationAuthorizationConflict{Field: "release_reference"}
		}
		return operationAuthorizationFromRow(row, true), nil
	case OperationAuthorizationSettled:
		return nil, ErrOperationAuthorizationNotOpen
	case OperationAuthorizationOpen:
	default:
		return nil, fmt.Errorf("operation authorization has invalid state %q", row.State)
	}
	released, err := q.ReleaseOperationAuthorization(ctx, gen.ReleaseOperationAuthorizationParams{
		MerchantID: merchantID.UUID(), OperationID: operationID,
		TerminalReference: releaseReference, ReleasedAt: s.now().UTC(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("operation authorization changed state under the payer lock")
	}
	if err != nil {
		return nil, err
	}
	return operationAuthorizationFromRow(released, false), nil
}

func operationAuthorizationFromRow(row gen.OpenrailsOperationAuthorization, replayed bool) *OperationAuthorization {
	var digest [sha256.Size]byte
	copy(digest[:], row.AuthorizationBodyDigest)
	terminalReference := ""
	if row.TerminalReference != nil {
		terminalReference = *row.TerminalReference
	}
	var settlementDigest [sha256.Size]byte
	copy(settlementDigest[:], row.SettlementBodyDigest)
	return &OperationAuthorization{
		OperationID:                     row.OperationID,
		MerchantID:                      row.MerchantID,
		Payer:                           identity.CustomerID(row.PayerID),
		RecordOwner:                     row.RecordOwner,
		LedgerAccountID:                 row.LedgerAccountID,
		AuthorizedUSDMicros:             row.AuthorizedUsdMicros,
		ClaimReference:                  row.ClaimReference,
		AuthorizationBody:               bytes.Clone(row.AuthorizationBodyBytes),
		AuthorizationBodySHA256:         digest,
		State:                           OperationAuthorizationState(row.State),
		TerminalReference:               terminalReference,
		SettlementProviderCostUSDMicros: row.SettlementProviderCostUsdMicros,
		SettlementRatedUSDMicros:        row.SettlementRatedUsdMicros,
		SettlementBody:                  bytes.Clone(row.SettlementBodyBytes),
		SettlementBodySHA256:            settlementDigest,
		CreatedAt:                       row.CreatedAt,
		ReleasedAt:                      row.ReleasedAt,
		SettledAt:                       row.SettledAt,
		Replayed:                        replayed,
	}
}
