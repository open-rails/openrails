package money

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	providerBillingMaxRawBodyBytes   = 16 << 20
	providerBillingMaxRecords        = 16_384
	providerBillingMaxQueryBytes     = 8 << 10
	providerBillingMaxLifecycleBytes = 64 << 10
)

type ProviderBillingQualificationState string

const (
	ProviderBillingQualificationPending  ProviderBillingQualificationState = "pending"
	ProviderBillingQualificationRefused  ProviderBillingQualificationState = "refused"
	ProviderBillingQualificationEligible ProviderBillingQualificationState = "eligible"
)

type ProviderBillingQualificationReason string

const (
	ProviderBillingAwaitingEqualObservation ProviderBillingQualificationReason = "awaiting_equal_observation"
	ProviderBillingAwaitingQuiescence       ProviderBillingQualificationReason = "awaiting_quiescence"
	ProviderBillingCoverageIncomplete       ProviderBillingQualificationReason = "coverage_incomplete"
	ProviderBillingObservationChanged       ProviderBillingQualificationReason = "observation_changed"
	ProviderBillingProviderEvidenceRefused  ProviderBillingQualificationReason = "provider_evidence_refused"
	ProviderBillingNegativeOrCorrective     ProviderBillingQualificationReason = "negative_or_corrective_record"
	ProviderBillingDecreasingProviderCost   ProviderBillingQualificationReason = "decreasing_provider_cost"
	ProviderBillingEligible                 ProviderBillingQualificationReason = "eligible"
)

var (
	ErrProviderBillingObservationConflict   = errors.New("provider_billing_observation_conflict")
	ErrProviderBillingQualificationRefused  = errors.New("provider_billing_qualification_refused")
	ErrProviderBillingQualificationNotFound = errors.New("provider_billing_qualification_not_found")
)

type ProviderBillingObservationConflict struct {
	Field string
}

func (e *ProviderBillingObservationConflict) Error() string {
	return fmt.Sprintf("provider billing evidence conflicts on %s", e.Field)
}

func (e *ProviderBillingObservationConflict) Unwrap() error {
	return ErrProviderBillingObservationConflict
}

type ProviderBillingLifecycleEvidence struct {
	Provider                 string
	ProviderResourceID       string
	ProviderLifetimeStart    time.Time
	ProviderLifetimeEnd      time.Time
	ProviderAbsentAt         time.Time
	ProviderAbsenceReference string
	BillingStopReference     string
	WindowsClosedAt          time.Time
	WindowsClosedReference   string
	LifecycleEvidenceBody    []byte
}

type ProviderBillingRecord struct {
	ProviderResourceID string
	BucketStart        time.Time
	AmountUSDMicros    int64
	TimeBilledMS       int64
}

type ProviderBillingEvidenceRefusalKind string

const (
	ProviderBillingRefusalSchemaAmbiguity  ProviderBillingEvidenceRefusalKind = "schema_ambiguity"
	ProviderBillingRefusalSubmicroAmount   ProviderBillingEvidenceRefusalKind = "submicro_amount"
	ProviderBillingRefusalAmountOverflow   ProviderBillingEvidenceRefusalKind = "amount_overflow"
	ProviderBillingRefusalResponseTooLarge ProviderBillingEvidenceRefusalKind = "response_too_large"
)

// ProviderBillingObservationRefusal is a stable typed refusal supplied by a
// provider adapter or SDK. OpenRails persists it but never parses provider raw
// bodies. RawBody may be empty when the provider response exceeded its bound.
type ProviderBillingObservationRefusal struct {
	Kind ProviderBillingEvidenceRefusalKind
}

type ProviderBillingObservationInput struct {
	OperationID     string
	ObservationID   string
	Lifecycle       ProviderBillingLifecycleEvidence
	NormalizedQuery string
	QueryStart      time.Time
	QueryEnd        time.Time
	RawBody         []byte
	Records         []ProviderBillingRecord
	Refusal         *ProviderBillingObservationRefusal
}

type ProviderBillingQualification struct {
	OperationID                    string
	MerchantID                     uuid.UUID
	Provider                       string
	ProviderResourceID             string
	ProviderLifetimeStart          time.Time
	ProviderLifetimeEnd            time.Time
	ProviderAbsentAt               time.Time
	ProviderAbsenceReference       string
	BillingStopReference           string
	WindowsClosedAt                time.Time
	WindowsClosedReference         string
	LifecycleEvidenceBody          []byte
	LifecycleEvidenceSHA256        [sha256.Size]byte
	Quiescence                     time.Duration
	State                          ProviderBillingQualificationState
	Reason                         ProviderBillingQualificationReason
	BaselineObservationID          string
	QualifiedObservationID         string
	QualifiedProviderCostUSDMicros *int64
	QualifiedAt                    *time.Time
	Authorization                  *OperationAuthorization
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
	Replayed                       bool
}

type normalizedProviderBillingRecord struct {
	ProviderResourceID string `json:"provider_resource_id"`
	BucketStart        string `json:"bucket_start"`
	AmountUSDMicros    int64  `json:"amount_usd_micros"`
	TimeBilledMS       int64  `json:"time_billed_ms"`
}

type preparedProviderBillingObservation struct {
	rawDigest               [sha256.Size]byte
	rawAvailable            bool
	normalizedRecords       []byte
	normalizedRecordsDigest [sha256.Size]byte
	providerCost            *int64
	hasNegative             bool
	refusalKind             *string
	coversLifetime          bool
}

// RecordProviderBillingObservationInTx appends one exact provider-neutral
// billing read, advances durable post-absence qualification, and is the only
// method allowed to invoke pass-through provider-cost settlement. It owns no
// transaction boundary and never calls a provider or request admission.
func (s *MoneyService) RecordProviderBillingObservationInTx(
	ctx context.Context,
	txDB *db.DB,
	in ProviderBillingObservationInput,
	quiescence time.Duration,
) (*ProviderBillingQualification, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if txDB == nil {
		return nil, fmt.Errorf("provider billing observation requires a bound transaction")
	}
	if quiescence < time.Second {
		return nil, fmt.Errorf("provider billing quiescence must be at least one second")
	}
	if quiescence%time.Second != 0 {
		return nil, fmt.Errorf("provider billing quiescence must use whole seconds")
	}
	if err := validateProviderBillingInput(in); err != nil {
		return nil, err
	}
	prepared, err := prepareProviderBillingObservation(in)
	if err != nil {
		return nil, err
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	txSvc := &MoneyService{db: txDB, clock: s.clock}
	q := txDB.Gen(ctx)
	authRow, err := q.GetOperationAuthorization(ctx, gen.GetOperationAuthorizationParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOperationAuthorizationNotFound
	}
	if err != nil {
		return nil, err
	}
	payer := identity.CustomerID(authRow.PayerID)
	if _, err := txSvc.lockBalance(ctx, q, payer, authRow.RecordOwner, operationAuthorizationCurrency); err != nil {
		return nil, err
	}
	authRow, err = q.GetOperationAuthorization(ctx, gen.GetOperationAuthorizationParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID,
	})
	if err != nil {
		return nil, err
	}

	lifecycleDigest := sha256.Sum256(in.Lifecycle.LifecycleEvidenceBody)
	qual, err := q.GetProviderBillingQualificationForUpdate(ctx, gen.GetProviderBillingQualificationForUpdateParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		qual, err = q.InsertProviderBillingQualification(ctx, gen.InsertProviderBillingQualificationParams{
			MerchantID:               merchantID.UUID(),
			OperationID:              in.OperationID,
			Provider:                 in.Lifecycle.Provider,
			ProviderResourceID:       in.Lifecycle.ProviderResourceID,
			ProviderLifetimeStart:    in.Lifecycle.ProviderLifetimeStart,
			ProviderLifetimeEnd:      in.Lifecycle.ProviderLifetimeEnd,
			ProviderAbsentAt:         in.Lifecycle.ProviderAbsentAt,
			ProviderAbsenceReference: in.Lifecycle.ProviderAbsenceReference,
			BillingStopReference:     in.Lifecycle.BillingStopReference,
			WindowsClosedAt:          in.Lifecycle.WindowsClosedAt,
			WindowsClosedReference:   in.Lifecycle.WindowsClosedReference,
			LifecycleEvidenceBytes:   in.Lifecycle.LifecycleEvidenceBody,
			LifecycleEvidenceDigest:  lifecycleDigest[:],
			QuiescenceSeconds:        int64(quiescence / time.Second),
		})
	}
	if err != nil {
		return nil, err
	}
	if err := replayProviderBillingLifecycle(qual, in.Lifecycle, lifecycleDigest); err != nil {
		return nil, err
	}

	if existing, getErr := q.GetProviderBillingObservation(ctx, gen.GetProviderBillingObservationParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID, ObservationID: in.ObservationID,
	}); getErr == nil {
		if err := replayProviderBillingObservation(existing, in, prepared); err != nil {
			return nil, err
		}
		result := providerBillingQualificationFromRow(qual, operationAuthorizationFromRow(authRow, false), true)
		return result, nil
	} else if !errors.Is(getErr, pgx.ErrNoRows) {
		return nil, getErr
	}

	if OperationAuthorizationState(authRow.State) != OperationAuthorizationOpen {
		return nil, ErrOperationAuthorizationNotOpen
	}
	if ProviderBillingQualificationState(qual.State) == ProviderBillingQualificationRefused {
		return nil, ErrProviderBillingQualificationRefused
	}
	if ProviderBillingQualificationState(qual.State) == ProviderBillingQualificationEligible {
		return nil, fmt.Errorf("provider billing qualification is eligible while authorization remains open")
	}

	now := s.now().UTC()
	if now.Before(qual.ProviderAbsentAt) {
		return nil, fmt.Errorf("provider billing observation precedes provider absence")
	}
	if now.Before(qual.WindowsClosedAt) {
		return nil, fmt.Errorf("provider billing observation precedes rental-window closure")
	}
	if in.QueryEnd.After(now) {
		return nil, fmt.Errorf("provider billing query ends after observation time")
	}
	state, reason, baselineID, qualifiedID, qualifiedCost, qualifiedAt, err := evaluateProviderBillingObservation(
		ctx, q, qual, in, prepared, now,
	)
	if err != nil {
		return nil, err
	}
	_, err = q.InsertProviderBillingObservation(ctx, gen.InsertProviderBillingObservationParams{
		MerchantID:              merchantID.UUID(),
		OperationID:             in.OperationID,
		ObservationID:           in.ObservationID,
		NormalizedQuery:         in.NormalizedQuery,
		QueryStart:              in.QueryStart,
		QueryEnd:                in.QueryEnd,
		RawBodyAvailable:        prepared.rawAvailable,
		RawBodyBytes:            in.RawBody,
		RawBodyDigest:           prepared.rawDigest[:],
		NormalizedRecordsBytes:  prepared.normalizedRecords,
		NormalizedRecordsDigest: nullableDigest(prepared),
		ProviderCostUsdMicros:   prepared.providerCost,
		HasNegativeRecord:       prepared.hasNegative,
		RefusalKind:             prepared.refusalKind,
		CoversLifetime:          prepared.coversLifetime,
		QualificationReason:     string(reason),
		ObservedAt:              now,
	})
	if err != nil {
		return nil, err
	}
	qual, err = q.UpdateProviderBillingQualification(ctx, gen.UpdateProviderBillingQualificationParams{
		State:                          string(state),
		Reason:                         string(reason),
		BaselineObservationID:          baselineID,
		QualifiedObservationID:         qualifiedID,
		QualifiedProviderCostUsdMicros: qualifiedCost,
		QualifiedAt:                    qualifiedAt,
		UpdatedAt:                      now,
		MerchantID:                     merchantID.UUID(),
		OperationID:                    in.OperationID,
	})
	if err != nil {
		return nil, err
	}

	auth := operationAuthorizationFromRow(authRow, false)
	if state == ProviderBillingQualificationEligible {
		body, err := providerBillingSettlementBody(ctx, q, qual)
		if err != nil {
			return nil, err
		}
		auth, err = txSvc.SettlePassThroughProviderCostInTx(ctx, txDB, PassThroughProviderCostSettlementInput{
			OperationID:           in.OperationID,
			ProviderCostUSDMicros: *qualifiedCost,
			SettlementBody:        body,
		})
		if err != nil {
			return nil, err
		}
	}
	return providerBillingQualificationFromRow(qual, auth, false), nil
}

func evaluateProviderBillingObservation(
	ctx context.Context,
	q *gen.Queries,
	qual gen.OpenrailsProviderBillingQualification,
	in ProviderBillingObservationInput,
	prepared preparedProviderBillingObservation,
	now time.Time,
) (ProviderBillingQualificationState, ProviderBillingQualificationReason, *string, *string, *int64, *time.Time, error) {
	if prepared.refusalKind != nil {
		return ProviderBillingQualificationRefused, ProviderBillingProviderEvidenceRefused, nil, nil, nil, nil, nil
	}
	if prepared.hasNegative {
		return ProviderBillingQualificationRefused, ProviderBillingNegativeOrCorrective, nil, nil, nil, nil, nil
	}
	if !prepared.coversLifetime {
		return ProviderBillingQualificationPending, ProviderBillingCoverageIncomplete, nil, nil, nil, nil, nil
	}
	if qual.BaselineObservationID == nil {
		id := in.ObservationID
		return ProviderBillingQualificationPending, ProviderBillingAwaitingEqualObservation, &id, nil, nil, nil, nil
	}
	baseline, err := q.GetProviderBillingObservation(ctx, gen.GetProviderBillingObservationParams{
		MerchantID: qual.MerchantID, OperationID: qual.OperationID, ObservationID: *qual.BaselineObservationID,
	})
	if err != nil {
		return "", "", nil, nil, nil, nil, fmt.Errorf("provider billing baseline observation: %w", err)
	}
	if baseline.ProviderCostUsdMicros == nil || prepared.providerCost == nil {
		return "", "", nil, nil, nil, nil, fmt.Errorf("provider billing baseline lacks normalized cost")
	}
	if *prepared.providerCost < *baseline.ProviderCostUsdMicros {
		return ProviderBillingQualificationRefused, ProviderBillingDecreasingProviderCost, qual.BaselineObservationID, nil, nil, nil, nil
	}
	if *prepared.providerCost != *baseline.ProviderCostUsdMicros ||
		baseline.NormalizedQuery != in.NormalizedQuery ||
		!baseline.QueryStart.Equal(in.QueryStart) ||
		!baseline.QueryEnd.Equal(in.QueryEnd) ||
		!bytes.Equal(prepared.normalizedRecordsDigest[:], baseline.NormalizedRecordsDigest) {
		id := in.ObservationID
		return ProviderBillingQualificationPending, ProviderBillingObservationChanged, &id, nil, nil, nil, nil
	}
	quiescence := time.Duration(qual.QuiescenceSeconds) * time.Second
	if now.Sub(baseline.ObservedAt) < quiescence {
		return ProviderBillingQualificationPending, ProviderBillingAwaitingQuiescence, qual.BaselineObservationID, nil, nil, nil, nil
	}
	qualifiedID := in.ObservationID
	cost := *prepared.providerCost
	qualifiedAt := now
	return ProviderBillingQualificationEligible, ProviderBillingEligible, qual.BaselineObservationID, &qualifiedID, &cost, &qualifiedAt, nil
}

func validateProviderBillingInput(in ProviderBillingObservationInput) error {
	if err := validateOperationAuthorizationText("operation_id", in.OperationID, operationAuthorizationMaxIDBytes); err != nil {
		return err
	}
	if err := validateOperationAuthorizationText("observation_id", in.ObservationID, operationAuthorizationMaxIDBytes); err != nil {
		return err
	}
	if err := validateOperationAuthorizationText("provider", in.Lifecycle.Provider, operationAuthorizationMaxPrincipalBytes); err != nil {
		return err
	}
	if err := validateOperationAuthorizationText("provider_resource_id", in.Lifecycle.ProviderResourceID, operationAuthorizationMaxPrincipalBytes); err != nil {
		return err
	}
	for field, ref := range map[string]string{
		"provider_absence_reference": in.Lifecycle.ProviderAbsenceReference,
		"billing_stop_reference":     in.Lifecycle.BillingStopReference,
		"windows_closed_reference":   in.Lifecycle.WindowsClosedReference,
	} {
		if err := validateOperationAuthorizationText(field, ref, operationAuthorizationMaxReferenceBytes); err != nil {
			return err
		}
	}
	for field, value := range map[string]time.Time{
		"provider_lifetime_start": in.Lifecycle.ProviderLifetimeStart,
		"provider_lifetime_end":   in.Lifecycle.ProviderLifetimeEnd,
		"provider_absent_at":      in.Lifecycle.ProviderAbsentAt,
		"windows_closed_at":       in.Lifecycle.WindowsClosedAt,
		"query_start":             in.QueryStart,
		"query_end":               in.QueryEnd,
	} {
		if value.IsZero() || !providerBillingTimeIsUTC(value) {
			return fmt.Errorf("%s must be a nonzero UTC time", field)
		}
		if value.Nanosecond()%1_000 != 0 {
			return fmt.Errorf("%s must use PostgreSQL-exact microsecond precision", field)
		}
	}
	if in.Lifecycle.ProviderLifetimeEnd.Before(in.Lifecycle.ProviderLifetimeStart) {
		return fmt.Errorf("provider_lifetime_end precedes provider_lifetime_start")
	}
	if in.Lifecycle.ProviderAbsentAt.Before(in.Lifecycle.ProviderLifetimeEnd) {
		return fmt.Errorf("provider_absent_at precedes provider_lifetime_end")
	}
	if in.Lifecycle.WindowsClosedAt.Before(in.Lifecycle.ProviderLifetimeEnd) {
		return fmt.Errorf("windows_closed_at precedes provider_lifetime_end")
	}
	if !in.QueryEnd.After(in.QueryStart) {
		return fmt.Errorf("query_end must be after query_start")
	}
	if err := validateOperationAuthorizationText("normalized_query", in.NormalizedQuery, providerBillingMaxQueryBytes); err != nil {
		return err
	}
	if len(in.Lifecycle.LifecycleEvidenceBody) == 0 || len(in.Lifecycle.LifecycleEvidenceBody) > providerBillingMaxLifecycleBytes {
		return fmt.Errorf("lifecycle_evidence_body must be 1..%d bytes", providerBillingMaxLifecycleBytes)
	}
	if len(in.RawBody) > providerBillingMaxRawBodyBytes {
		return fmt.Errorf("provider billing raw body exceeds %d bytes", providerBillingMaxRawBodyBytes)
	}
	if in.Refusal == nil {
		if len(in.RawBody) == 0 {
			return fmt.Errorf("successful provider billing observation requires raw body")
		}
		if len(in.Records) > providerBillingMaxRecords {
			return fmt.Errorf("provider billing observation exceeds %d records", providerBillingMaxRecords)
		}
	} else {
		if len(in.Records) != 0 {
			return fmt.Errorf("refused provider billing observation cannot include normalized records")
		}
		if err := validateOperationAuthorizationText("refusal_kind", string(in.Refusal.Kind), operationAuthorizationMaxPrincipalBytes); err != nil {
			return err
		}
		switch in.Refusal.Kind {
		case ProviderBillingRefusalSchemaAmbiguity,
			ProviderBillingRefusalSubmicroAmount,
			ProviderBillingRefusalAmountOverflow:
			if len(in.RawBody) == 0 {
				return fmt.Errorf("provider billing refusal %q requires exact bounded raw body", in.Refusal.Kind)
			}
		case ProviderBillingRefusalResponseTooLarge:
			if len(in.RawBody) != 0 {
				return fmt.Errorf("provider billing refusal %q cannot retain partial raw body", in.Refusal.Kind)
			}
		default:
			return fmt.Errorf("unsupported provider billing refusal kind %q", in.Refusal.Kind)
		}
	}
	return nil
}

func prepareProviderBillingObservation(in ProviderBillingObservationInput) (preparedProviderBillingObservation, error) {
	prepared := preparedProviderBillingObservation{
		rawDigest:      sha256.Sum256(in.RawBody),
		rawAvailable:   len(in.RawBody) > 0,
		coversLifetime: !in.QueryStart.After(in.Lifecycle.ProviderLifetimeStart) && !in.QueryEnd.Before(in.Lifecycle.ProviderLifetimeEnd),
	}
	if in.Refusal != nil {
		kind := string(in.Refusal.Kind)
		prepared.refusalKind = &kind
		prepared.coversLifetime = false
		return prepared, nil
	}
	records := make([]normalizedProviderBillingRecord, len(in.Records))
	var total int64
	for i, record := range in.Records {
		if record.ProviderResourceID != in.Lifecycle.ProviderResourceID {
			return prepared, fmt.Errorf("provider billing record %d names foreign resource %q", i, record.ProviderResourceID)
		}
		if record.BucketStart.IsZero() || !providerBillingTimeIsUTC(record.BucketStart) {
			return prepared, fmt.Errorf("provider billing record %d bucket_start must be nonzero UTC", i)
		}
		if record.BucketStart.Nanosecond()%1_000 != 0 {
			return prepared, fmt.Errorf("provider billing record %d bucket_start must use PostgreSQL-exact microsecond precision", i)
		}
		if record.TimeBilledMS < 0 {
			return prepared, fmt.Errorf("provider billing record %d has negative time_billed_ms", i)
		}
		if (record.AmountUSDMicros > 0 && total > math.MaxInt64-record.AmountUSDMicros) ||
			(record.AmountUSDMicros < 0 && total < math.MinInt64-record.AmountUSDMicros) {
			return prepared, fmt.Errorf("provider billing record %d makes total USD micros overflow", i)
		}
		total += record.AmountUSDMicros
		prepared.hasNegative = prepared.hasNegative || record.AmountUSDMicros < 0
		records[i] = normalizedProviderBillingRecord{
			ProviderResourceID: record.ProviderResourceID,
			BucketStart:        record.BucketStart.UTC().Format(time.RFC3339Nano),
			AmountUSDMicros:    record.AmountUSDMicros,
			TimeBilledMS:       record.TimeBilledMS,
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].BucketStart != records[j].BucketStart {
			return records[i].BucketStart < records[j].BucketStart
		}
		if records[i].ProviderResourceID != records[j].ProviderResourceID {
			return records[i].ProviderResourceID < records[j].ProviderResourceID
		}
		if records[i].AmountUSDMicros != records[j].AmountUSDMicros {
			return records[i].AmountUSDMicros < records[j].AmountUSDMicros
		}
		return records[i].TimeBilledMS < records[j].TimeBilledMS
	})
	normalized, err := json.Marshal(records)
	if err != nil {
		return prepared, fmt.Errorf("canonicalize provider billing records: %w", err)
	}
	if len(normalized) > providerBillingMaxRawBodyBytes {
		return prepared, fmt.Errorf("normalized provider billing records exceed %d bytes", providerBillingMaxRawBodyBytes)
	}
	prepared.normalizedRecords = normalized
	prepared.normalizedRecordsDigest = sha256.Sum256(normalized)
	prepared.providerCost = &total
	return prepared, nil
}

func providerBillingTimeIsUTC(value time.Time) bool {
	_, offset := value.Zone()
	return offset == 0
}

func nullableDigest(prepared preparedProviderBillingObservation) []byte {
	if prepared.refusalKind != nil {
		return nil
	}
	return prepared.normalizedRecordsDigest[:]
}

func replayProviderBillingLifecycle(row gen.OpenrailsProviderBillingQualification, in ProviderBillingLifecycleEvidence, digest [sha256.Size]byte) error {
	checks := []struct {
		field string
		same  bool
	}{
		{"provider", row.Provider == in.Provider},
		{"provider_resource_id", row.ProviderResourceID == in.ProviderResourceID},
		{"provider_lifetime_start", row.ProviderLifetimeStart.Equal(in.ProviderLifetimeStart)},
		{"provider_lifetime_end", row.ProviderLifetimeEnd.Equal(in.ProviderLifetimeEnd)},
		{"provider_absent_at", row.ProviderAbsentAt.Equal(in.ProviderAbsentAt)},
		{"provider_absence_reference", row.ProviderAbsenceReference == in.ProviderAbsenceReference},
		{"billing_stop_reference", row.BillingStopReference == in.BillingStopReference},
		{"windows_closed_at", row.WindowsClosedAt.Equal(in.WindowsClosedAt)},
		{"windows_closed_reference", row.WindowsClosedReference == in.WindowsClosedReference},
		{"lifecycle_evidence_body", bytes.Equal(row.LifecycleEvidenceBytes, in.LifecycleEvidenceBody)},
		{"lifecycle_evidence_sha256", bytes.Equal(row.LifecycleEvidenceDigest, digest[:])},
	}
	for _, check := range checks {
		if !check.same {
			return &ProviderBillingObservationConflict{Field: check.field}
		}
	}
	return nil
}

func replayProviderBillingObservation(row gen.OpenrailsProviderBillingObservation, in ProviderBillingObservationInput, prepared preparedProviderBillingObservation) error {
	checks := []struct {
		field string
		same  bool
	}{
		{"normalized_query", row.NormalizedQuery == in.NormalizedQuery},
		{"query_start", row.QueryStart.Equal(in.QueryStart)},
		{"query_end", row.QueryEnd.Equal(in.QueryEnd)},
		{"raw_body", bytes.Equal(row.RawBodyBytes, in.RawBody)},
		{"raw_body_sha256", bytes.Equal(row.RawBodyDigest, prepared.rawDigest[:])},
		{"normalized_records", bytes.Equal(row.NormalizedRecordsBytes, prepared.normalizedRecords)},
		{"normalized_records_sha256", bytes.Equal(row.NormalizedRecordsDigest, nullableDigest(prepared))},
		{"provider_cost_usd_micros", equalOptionalInt64(row.ProviderCostUsdMicros, prepared.providerCost)},
		{"negative_record", row.HasNegativeRecord == prepared.hasNegative},
		{"refusal_kind", equalOptionalString(row.RefusalKind, prepared.refusalKind)},
	}
	for _, check := range checks {
		if !check.same {
			return &ProviderBillingObservationConflict{Field: check.field}
		}
	}
	return nil
}

func equalOptionalInt64(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func equalOptionalString(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

type providerBillingSettlementManifest struct {
	Contract                       string                               `json:"contract"`
	OperationID                    string                               `json:"operation_id"`
	Provider                       string                               `json:"provider"`
	ProviderResourceID             string                               `json:"provider_resource_id"`
	ProviderLifetimeStart          string                               `json:"provider_lifetime_start"`
	ProviderLifetimeEnd            string                               `json:"provider_lifetime_end"`
	ProviderAbsentAt               string                               `json:"provider_absent_at"`
	ProviderAbsenceReference       string                               `json:"provider_absence_reference"`
	BillingStopReference           string                               `json:"billing_stop_reference"`
	WindowsClosedAt                string                               `json:"windows_closed_at"`
	WindowsClosedReference         string                               `json:"windows_closed_reference"`
	LifecycleEvidenceSHA256        string                               `json:"lifecycle_evidence_sha256"`
	BaselineObservation            providerBillingSettlementObservation `json:"baseline_observation"`
	QualifiedObservation           providerBillingSettlementObservation `json:"qualified_observation"`
	QualifiedProviderCostUSDMicros int64                                `json:"qualified_provider_cost_usd_micros"`
	QuiescenceSeconds              int64                                `json:"quiescence_seconds"`
	QualifiedAt                    string                               `json:"qualified_at"`
}

type providerBillingSettlementObservation struct {
	ObservationID           string `json:"observation_id"`
	NormalizedQuerySHA256   string `json:"normalized_query_sha256"`
	QueryStart              string `json:"query_start"`
	QueryEnd                string `json:"query_end"`
	RawBodySHA256           string `json:"raw_body_sha256"`
	NormalizedRecordsSHA256 string `json:"normalized_records_sha256"`
}

func providerBillingSettlementBody(ctx context.Context, q *gen.Queries, row gen.OpenrailsProviderBillingQualification) ([]byte, error) {
	if row.BaselineObservationID == nil || row.QualifiedObservationID == nil ||
		row.QualifiedProviderCostUsdMicros == nil || row.QualifiedAt == nil {
		return nil, fmt.Errorf("eligible provider billing qualification is incomplete")
	}
	baseline, err := q.GetProviderBillingObservation(ctx, gen.GetProviderBillingObservationParams{
		MerchantID: row.MerchantID, OperationID: row.OperationID, ObservationID: *row.BaselineObservationID,
	})
	if err != nil {
		return nil, fmt.Errorf("load baseline provider billing evidence: %w", err)
	}
	qualified, err := q.GetProviderBillingObservation(ctx, gen.GetProviderBillingObservationParams{
		MerchantID: row.MerchantID, OperationID: row.OperationID, ObservationID: *row.QualifiedObservationID,
	})
	if err != nil {
		return nil, fmt.Errorf("load qualified provider billing evidence: %w", err)
	}
	body, err := json.Marshal(providerBillingSettlementManifest{
		Contract:                       "openrails/pass-through-provider-cost",
		OperationID:                    row.OperationID,
		Provider:                       row.Provider,
		ProviderResourceID:             row.ProviderResourceID,
		ProviderLifetimeStart:          row.ProviderLifetimeStart.UTC().Format(time.RFC3339Nano),
		ProviderLifetimeEnd:            row.ProviderLifetimeEnd.UTC().Format(time.RFC3339Nano),
		ProviderAbsentAt:               row.ProviderAbsentAt.UTC().Format(time.RFC3339Nano),
		ProviderAbsenceReference:       row.ProviderAbsenceReference,
		BillingStopReference:           row.BillingStopReference,
		WindowsClosedAt:                row.WindowsClosedAt.UTC().Format(time.RFC3339Nano),
		WindowsClosedReference:         row.WindowsClosedReference,
		LifecycleEvidenceSHA256:        hex.EncodeToString(row.LifecycleEvidenceDigest),
		BaselineObservation:            providerBillingSettlementObservationFromRow(baseline),
		QualifiedObservation:           providerBillingSettlementObservationFromRow(qualified),
		QualifiedProviderCostUSDMicros: *row.QualifiedProviderCostUsdMicros,
		QuiescenceSeconds:              row.QuiescenceSeconds,
		QualifiedAt:                    row.QualifiedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, fmt.Errorf("author provider billing settlement body: %w", err)
	}
	return body, nil
}

func providerBillingSettlementObservationFromRow(row gen.OpenrailsProviderBillingObservation) providerBillingSettlementObservation {
	queryDigest := sha256.Sum256([]byte(row.NormalizedQuery))
	return providerBillingSettlementObservation{
		ObservationID:           row.ObservationID,
		NormalizedQuerySHA256:   hex.EncodeToString(queryDigest[:]),
		QueryStart:              row.QueryStart.UTC().Format(time.RFC3339Nano),
		QueryEnd:                row.QueryEnd.UTC().Format(time.RFC3339Nano),
		RawBodySHA256:           hex.EncodeToString(row.RawBodyDigest),
		NormalizedRecordsSHA256: hex.EncodeToString(row.NormalizedRecordsDigest),
	}
}

func providerBillingQualificationFromRow(row gen.OpenrailsProviderBillingQualification, auth *OperationAuthorization, replayed bool) *ProviderBillingQualification {
	var lifecycleDigest [sha256.Size]byte
	copy(lifecycleDigest[:], row.LifecycleEvidenceDigest)
	return &ProviderBillingQualification{
		OperationID:                    row.OperationID,
		MerchantID:                     row.MerchantID,
		Provider:                       row.Provider,
		ProviderResourceID:             row.ProviderResourceID,
		ProviderLifetimeStart:          row.ProviderLifetimeStart,
		ProviderLifetimeEnd:            row.ProviderLifetimeEnd,
		ProviderAbsentAt:               row.ProviderAbsentAt,
		ProviderAbsenceReference:       row.ProviderAbsenceReference,
		BillingStopReference:           row.BillingStopReference,
		WindowsClosedAt:                row.WindowsClosedAt,
		WindowsClosedReference:         row.WindowsClosedReference,
		LifecycleEvidenceBody:          append([]byte(nil), row.LifecycleEvidenceBytes...),
		LifecycleEvidenceSHA256:        lifecycleDigest,
		Quiescence:                     time.Duration(row.QuiescenceSeconds) * time.Second,
		State:                          ProviderBillingQualificationState(row.State),
		Reason:                         ProviderBillingQualificationReason(row.Reason),
		BaselineObservationID:          providerBillingOptionalString(row.BaselineObservationID),
		QualifiedObservationID:         providerBillingOptionalString(row.QualifiedObservationID),
		QualifiedProviderCostUSDMicros: row.QualifiedProviderCostUsdMicros,
		QualifiedAt:                    row.QualifiedAt,
		Authorization:                  auth,
		CreatedAt:                      row.CreatedAt,
		UpdatedAt:                      row.UpdatedAt,
		Replayed:                       replayed,
	}
}

func providerBillingOptionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// GetProviderBillingQualification reads the bound merchant's durable
// qualification state. It does not attempt settlement.
func (s *MoneyService) GetProviderBillingQualification(ctx context.Context, operationID string) (*ProviderBillingQualification, error) {
	if err := validateOperationAuthorizationText("operation_id", operationID, operationAuthorizationMaxIDBytes); err != nil {
		return nil, err
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	q := s.db.Gen(ctx)
	row, err := q.GetProviderBillingQualification(ctx, gen.GetProviderBillingQualificationParams{
		MerchantID: merchantID.UUID(), OperationID: operationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderBillingQualificationNotFound
	}
	if err != nil {
		return nil, err
	}
	auth, err := s.GetOperationAuthorization(ctx, operationID)
	if err != nil {
		return nil, err
	}
	return providerBillingQualificationFromRow(row, auth, false), nil
}
