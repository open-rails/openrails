package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/service"
)

// The Tx methods run the same commands as the Client inside tx, a
// transaction from the host's pool on this engine's database, for running
// charges and admissions that must commit atomically with host rows.
// OpenRails binds mid, the handle's merchant, to tx and never commits or rolls
// it back; after an error the host rolls back.

// StartRunningChargeTx starts a charge, or answers the one its reference
// names; commit it with the host's fact that the work is ready.
func (e *Engine) StartRunningChargeTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, params billing.StartRunningChargeParams) (*billing.RunningCharge, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	charge, _, err := e.svc.StartRunningChargeTx(ctx, tx, params)
	return charge, err
}

// StopRunningChargeTx stops a charge and posts the rest of it; commit it with
// the host's fact that the work was released.
func (e *Engine) StopRunningChargeTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, reference string, params billing.StopRunningChargeParams) (*billing.RunningCharge, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	return e.svc.StopRunningChargeTx(ctx, tx, reference, params)
}

// AdmitTx admits each request inside tx; commit it with the host's request
// rows. A refused request is a verdict and writes nothing.
func (e *Engine) AdmitTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, requests []billing.AdmitParams) ([]billing.AdmissionVerdict, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	return handlers.AdmitVerdicts(ctx, requests, func(billing.CustomerID) bool { return true },
		func(ctx context.Context, in service.AdmitInput) (*billing.Admission, error) {
			return e.svc.AdmitTx(ctx, tx, in)
		}), nil
}

// CaptureAdmissionsTx settles each admission inside tx; commit it with the
// host's record of the finished work.
func (e *Engine) CaptureAdmissionsTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, items []billing.CaptureAdmissionParams) ([]billing.CaptureResult, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	return handlers.CaptureResults(ctx, items, nil, func(ctx context.Context, params billing.CaptureAdmissionParams) (*billing.CaptureReceipt, error) {
		return e.svc.CaptureAdmissionTx(ctx, tx, params)
	}), nil
}

// ReleaseAdmissionsTx releases each admission inside tx; commit it with the
// host's record of the failed work.
func (e *Engine) ReleaseAdmissionsTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, requestIDs []string) ([]billing.AdmissionResult, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	out := make([]billing.AdmissionResult, len(requestIDs))
	for i, id := range requestIDs {
		out[i] = handlers.ReleaseResult(ctx, strings.TrimSpace(id), func(ctx context.Context, requestID string) (*billing.Admission, error) {
			return e.svc.ReleaseAdmissionTx(ctx, tx, requestID)
		})
	}
	return out, nil
}

// bind pins mid for a call inside tx, after reading it live through tx: an
// unknown, deleted or archived merchant is billing.ErrMerchantUnresolved.
func (e *Engine) bind(ctx context.Context, mid billing.MerchantID, tx pgx.Tx) (context.Context, error) {
	if tx == nil {
		return ctx, fmt.Errorf("openrails: a transaction is required")
	}
	row, err := e.App.Runtime.DB.NewWithPgxTx(tx).Gen(ctx).GetMerchantDirectoryByID(ctx, mid.UUID())
	switch {
	case errors.Is(err, pgx.ErrNoRows), err == nil && row.Status != string(merchants.StatusActive):
		return ctx, fmt.Errorf("%w: %s is not a live merchant of this engine", billing.ErrMerchantUnresolved, mid)
	case err != nil:
		return ctx, fmt.Errorf("openrails: read merchant %s: %w", mid, err)
	}
	return merchant.WithID(ctx, mid), nil
}
