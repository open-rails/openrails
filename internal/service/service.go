package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/money"
)

// Service is the exported, in-process billing API.
//
// It is intended for embedded hosts that want to call billing logic directly, without going
// through the HTTP handlers. The standalone HTTP server should treat its routes as thin
// adapters over this API.
type Service struct {
	rt                   *app.Runtime
	catalogTx            *db.DB
	localCatalogOnly     bool
	catalogWriteLocked   bool
	catalogCommittedWork *[]func(context.Context, *Service)
	catalogPreparedLinks map[string]map[string]map[string]string
}

func New(rt *app.Runtime) (*Service, error) {
	if rt == nil {
		return nil, fmt.Errorf("billing service: runtime is nil")
	}
	if rt.MoneyService == nil {
		return nil, fmt.Errorf("billing service: money service unavailable")
	}
	if rt.EntitlementService == nil {
		return nil, fmt.Errorf("billing service: entitlement service unavailable")
	}
	return &Service{rt: rt}, nil
}

func (s *Service) now() time.Time {
	if s != nil && s.rt != nil && s.rt.Clock != nil {
		return s.rt.Clock.Now()
	}
	return time.Now()
}

var ErrInsufficientCredits = money.ErrInsufficientCredits

// ErrIdempotencyKeyReused is the money-write refusal a caller can act on: the
// key already committed, and THIS retry carries different charging terms
// (or#891). It is re-exported here because the rule lives in internal/ — a host
// could see the failure but not name it, so it could not tell a caller bug
// apart from an engine fault. Wrapped errors carry the detail
// (money.IdempotencyConflict: which field, committed vs retried).
var ErrIdempotencyKeyReused = money.ErrIdempotencyKeyReused

// CaptureAdmission settles an admitted request. The first capture fixes its
// amount and usage terms; an exact retry returns the original receipt.
func (s *Service) CaptureAdmission(ctx context.Context, requestID string, params billing.CaptureParams) (*billing.CaptureReceipt, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, fmt.Errorf("request_id required")
	}
	return s.moneyService().CaptureAdmission(ctx, requestID, params.Amount, params.Usage)
}

// GetAdmission reads an allowed admission and its hold.
func (s *Service) GetAdmission(ctx context.Context, requestID string) (*billing.Admission, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	row, err := spendgate.New(s.rt.DB).Get(ctx, strings.TrimSpace(requestID))
	if err != nil {
		return nil, err
	}
	return admissionFromOperation(row, s.now()), nil
}

// AdmissionCustomer resolves ownership from the durable operation before route authorization.
func (s *Service) AdmissionCustomer(ctx context.Context, requestID string) (identity.CustomerID, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return identity.CustomerID{}, err
	}
	defer release()
	row, err := spendgate.New(s.rt.DB).Get(ctx, requestID)
	return identity.CustomerID(row.CustomerID), err
}

// ReleaseAdmission frees an open hold. Releasing a released admission
// returns it unchanged.
func (s *Service) ReleaseAdmission(ctx context.Context, requestID string) (*billing.Admission, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, fmt.Errorf("request_id required")
	}
	gate := spendgate.New(s.rt.DB)
	gate.SetClock(s.now)
	if err := gate.Release(ctx, requestID); err != nil {
		return nil, err
	}
	row, err := gate.Get(ctx, requestID)
	if err != nil {
		return nil, err
	}
	return admissionFromOperation(row, s.now()), nil
}

// ExtendAdmission moves an open hold's deadline to expiresAt.
func (s *Service) ExtendAdmission(ctx context.Context, requestID string, expiresAt time.Time) (*billing.Admission, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, fmt.Errorf("request_id required")
	}
	if expiresAt.IsZero() {
		return nil, ErrHoldDeadlineRequired
	}
	gate := spendgate.New(s.rt.DB)
	gate.SetClock(s.now)
	err = gate.Extend(ctx, requestID, expiresAt)
	if errors.Is(err, spendgate.ErrExpired) {
		return nil, ErrHoldDeadlinePassed
	}
	if errors.Is(err, spendgate.ErrNotFound) {
		return nil, ErrHoldNotFound
	}
	if err != nil {
		return nil, err
	}
	row, err := gate.Get(ctx, requestID)
	if err != nil {
		return nil, err
	}
	return admissionFromOperation(row, s.now()), nil
}

func (s *Service) ListActiveEntitlements(ctx context.Context, userID string, at time.Time) ([]string, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().ListActiveEntitlements(ctx, userID, at.UTC())
}

func (s *Service) ListActiveEntitlementsForCustomer(ctx context.Context, tenantSubjectID identity.CustomerID, at time.Time) ([]string, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if tenantSubjectID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().ListActiveEntitlementsByCustomer(ctx, tenantSubjectID.UUID(), at.UTC())
}

func (s *Service) IsCustomerEntitled(ctx context.Context, tenantSubjectID identity.CustomerID, entitlement string, at time.Time) (bool, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return false, pinErr
	}
	defer release()

	if tenantSubjectID.IsZero() {
		return false, fmt.Errorf("customer_id required")
	}
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return false, fmt.Errorf("entitlement required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().IsCustomerEntitled(ctx, tenantSubjectID.UUID(), entitlement, at.UTC())
}

func (s *Service) HasActiveIndefiniteEntitlementForCustomer(ctx context.Context, tenantSubjectID identity.CustomerID, entitlement string, at time.Time) (bool, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return false, pinErr
	}
	defer release()

	if tenantSubjectID.IsZero() {
		return false, fmt.Errorf("customer_id required")
	}
	entitlement = strings.TrimSpace(entitlement)
	if entitlement == "" {
		return false, fmt.Errorf("entitlement required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().HasActiveIndefiniteByCustomer(ctx, tenantSubjectID.UUID(), entitlement, at.UTC())
}

type EntitlementRecord struct {
	ID           uuid.UUID
	CustomerID   uuid.UUID
	UserID       string
	Entitlement  string
	StartAt      time.Time
	EndAt        *time.Time
	SourceID     *uuid.UUID
	SourceType   string
	RevokedAt    *time.Time
	RevokeReason *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (s *Service) ListActiveEntitlementRecords(ctx context.Context, userID string, at time.Time) ([]EntitlementRecord, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	records, err := s.entitlementService().ListActiveRecords(ctx, userID, at.UTC())
	if err != nil {
		return nil, err
	}
	out := make([]EntitlementRecord, 0, len(records))
	for _, e := range records {
		reason := (*string)(nil)
		if e.RevokeReason != nil {
			v := string(*e.RevokeReason)
			reason = &v
		}
		out = append(out, EntitlementRecord{
			ID:           e.ID,
			CustomerID:   e.CustomerID,
			UserID:       e.CustomerID.String(),
			Entitlement:  e.Entitlement,
			StartAt:      e.StartAt,
			EndAt:        e.EndAt,
			SourceID:     e.SourceID,
			SourceType:   string(e.SourceType),
			RevokedAt:    e.RevokedAt,
			RevokeReason: reason,
			CreatedAt:    e.CreatedAt,
			UpdatedAt:    e.UpdatedAt,
		})
	}
	return out, nil
}

func (s *Service) ListActiveEntitlementRecordsForCustomer(ctx context.Context, tenantSubjectID identity.CustomerID, at time.Time) ([]EntitlementRecord, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if tenantSubjectID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	records, err := s.entitlementService().ListActiveRecordsByCustomer(ctx, tenantSubjectID.UUID(), at.UTC())
	if err != nil {
		return nil, err
	}
	out := make([]EntitlementRecord, 0, len(records))
	for _, e := range records {
		reason := (*string)(nil)
		if e.RevokeReason != nil {
			v := string(*e.RevokeReason)
			reason = &v
		}
		out = append(out, EntitlementRecord{
			ID:           e.ID,
			CustomerID:   e.CustomerID,
			UserID:       e.CustomerID.String(),
			Entitlement:  e.Entitlement,
			StartAt:      e.StartAt,
			EndAt:        e.EndAt,
			SourceID:     e.SourceID,
			SourceType:   string(e.SourceType),
			RevokedAt:    e.RevokedAt,
			RevokeReason: reason,
			CreatedAt:    e.CreatedAt,
			UpdatedAt:    e.UpdatedAt,
		})
	}
	return out, nil
}

// EntitlementsBatchMaxSubjects bounds one batch read (#354); over-cap is an
// explicit error at the edge, never a silent truncation. Shared by the
// standalone handler and the embedded transport.
const EntitlementsBatchMaxSubjects = 500

// ListActiveEntitlementRecordsByExternalSubjects (#354): active rows for many
// external subjects of one (ambient-merchant, issuer) in one query, grouped by
// subject; subjects with no rows are absent. Callers trim/dedupe/cap.
// ListCustomersWithEntitlement is the reverse lookup (#535): customer ids holding
// an active window of `entitlement` for the merchant, keyset-paginated (afterID
// exclusive; uuid.Nil starts). A zero `at` means now.
func (s *Service) ListCustomersWithEntitlement(ctx context.Context, entitlement string, at time.Time, afterID uuid.UUID, limit int) ([]uuid.UUID, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.entitlementService().ListCustomersWithEntitlement(ctx, entitlement, at.UTC(), afterID, limit)
}

func (s *Service) ListActiveEntitlementRecordsByExternalSubjects(ctx context.Context, subjects []string, at time.Time) (map[string][]EntitlementRecord, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if len(subjects) == 0 {
		return map[string][]EntitlementRecord{}, nil
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	grouped, err := s.entitlementService().ListActiveRecordsByExternalSubjects(ctx, subjects, at.UTC())
	if err != nil {
		return nil, err
	}
	out := make(map[string][]EntitlementRecord, len(grouped))
	for subject, records := range grouped {
		rs := make([]EntitlementRecord, 0, len(records))
		for _, e := range records {
			reason := (*string)(nil)
			if e.RevokeReason != nil {
				v := string(*e.RevokeReason)
				reason = &v
			}
			rs = append(rs, EntitlementRecord{
				ID:           e.ID,
				CustomerID:   e.CustomerID,
				UserID:       e.CustomerID.String(),
				Entitlement:  e.Entitlement,
				StartAt:      e.StartAt,
				EndAt:        e.EndAt,
				SourceID:     e.SourceID,
				SourceType:   string(e.SourceType),
				RevokedAt:    e.RevokedAt,
				RevokeReason: reason,
				CreatedAt:    e.CreatedAt,
				UpdatedAt:    e.UpdatedAt,
			})
		}
		out[subject] = rs
	}
	return out, nil
}
