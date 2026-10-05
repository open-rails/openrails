package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

// spendGate is the admission gate on the engine's clock: an admission is
// looked up within the retained range as that clock counts it.
func (s *Service) spendGate() *spendgate.Gate {
	gate := spendgate.New(s.rt.DB)
	gate.SetClock(s.now)
	return gate
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

// ErrUsageOutsideIngestWindow is returned by RecordUsage when occurred_at is
// older than the ingest window or ahead of the clock.
var ErrUsageOutsideIngestWindow = money.ErrUsageOutsideIngestWindow

// CaptureAdmission settles an admitted request. The first capture fixes its
// amount and usage terms; an exact retry returns the original receipt.
func (s *Service) CaptureAdmission(ctx context.Context, requestID string, params billing.CaptureAdmissionParams) (*billing.CaptureReceipt, error) {
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
	row, err := s.spendGate().Get(ctx, strings.TrimSpace(requestID))
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
	row, err := s.spendGate().Get(ctx, requestID)
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
	gate := s.spendGate()
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
	gate := s.spendGate()
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
