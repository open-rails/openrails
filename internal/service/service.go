package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
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
	catalogPreparedLinks map[[2]string]map[string]map[string]string
	// catalogPreparedProviders are an application's provider prices, read
	// from their providers.
	catalogPreparedProviders map[[2]string]*providerPrice
	// catalogKeyActor attributes key edits made inside a catalog application.
	catalogKeyActor string
	// catalogKeyChanges collects the key changes of existing products made
	// inside a catalog application, for its receipt.
	catalogKeyChanges *[]billing.EntitlementChange
	// catalogBefore is the catalog as the mutation found it, under its lock.
	catalogBefore catalogState
	// catalogApplying marks a document application: it records ownership
	// itself, as the apply manager.
	catalogApplying bool
	// catalogApplicationID is the application's id, for its history.
	catalogApplicationID string
	// catalogRenames are the key renames the mutation made, which history
	// records as renames, not as a removal and an addition.
	catalogRenames []catalogKeyRename
	Log            *slog.Logger
}

func New(logger *slog.Logger, rt *app.Runtime) (*Service, error) {
	if rt == nil {
		return nil, fmt.Errorf("billing service: runtime is nil")
	}
	if rt.MoneyService == nil {
		return nil, fmt.Errorf("billing service: money service unavailable")
	}
	if rt.EntitlementService == nil {
		return nil, fmt.Errorf("billing service: entitlement service unavailable")
	}
	return &Service{rt: rt, Log: logger}, nil
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

// ErrUsageOutsideIngestWindow refuses usage whose occurred_at is older than
// the ingest window or ahead of the clock.
var ErrUsageOutsideIngestWindow = money.ErrUsageOutsideIngestWindow
