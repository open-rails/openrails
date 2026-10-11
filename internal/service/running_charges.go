package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
)

// Starting and stopping a running charge exist in two forms with identical
// semantics: the plain form commits its own merchant transaction (the Client
// routes), and the Tx form rides a transaction owned by an embedding host,
// which alone commits or rolls it back.

// inRunningChargeTx binds the merchant to tx and runs fn on it.
func (s *Service) inRunningChargeTx(ctx context.Context, tx pgx.Tx, fn func(context.Context, *db.DB) error) error {
	rt, err := s.runtime()
	if err != nil {
		return err
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	ctx, txDB, err := rt.DB.BindMerchantTx(ctx, tx, merchantID)
	if err != nil {
		return err
	}
	return fn(ctx, txDB)
}

// ownRunningChargeTx runs fn in a merchant transaction of its own.
func (s *Service) ownRunningChargeTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	rt, err := s.runtime()
	if err != nil {
		return err
	}
	return rt.DB.MerchantTx(ctx, fn)
}

// StartRunningCharge starts a charge, or answers the one its reference names;
// started is false for one that already existed.
func (s *Service) StartRunningCharge(ctx context.Context, params billing.StartRunningChargeParams) (charge *billing.RunningCharge, started bool, err error) {
	err = s.ownRunningChargeTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		charge, started, err = s.StartRunningChargeTx(ctx, tx, params)
		return err
	})
	return charge, started, err
}

// StartRunningChargeTx is the host-transaction form.
func (s *Service) StartRunningChargeTx(ctx context.Context, tx pgx.Tx, params billing.StartRunningChargeParams) (charge *billing.RunningCharge, started bool, err error) {
	err = s.inRunningChargeTx(ctx, tx, func(ctx context.Context, txDB *db.DB) error {
		charge, started, err = s.moneyService().StartRunningChargeInTx(ctx, txDB, params)
		return err
	})
	return charge, started, err
}

// StopRunningCharge stops a charge and posts the rest of it.
func (s *Service) StopRunningCharge(ctx context.Context, reference string, params billing.StopRunningChargeParams) (charge *billing.RunningCharge, err error) {
	err = s.ownRunningChargeTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		charge, err = s.StopRunningChargeTx(ctx, tx, reference, params)
		return err
	})
	return charge, err
}

// StopRunningChargeTx is the host-transaction form.
func (s *Service) StopRunningChargeTx(ctx context.Context, tx pgx.Tx, reference string, params billing.StopRunningChargeParams) (charge *billing.RunningCharge, err error) {
	err = s.inRunningChargeTx(ctx, tx, func(ctx context.Context, txDB *db.DB) error {
		charge, err = s.moneyService().StopRunningChargeInTx(ctx, txDB, reference, params)
		return err
	})
	return charge, err
}

// GetRunningCharge reads one charge by the host's reference.
func (s *Service) GetRunningCharge(ctx context.Context, reference string) (*billing.RunningCharge, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.moneyService().GetRunningCharge(ctx, reference)
}

// ListRunningCharges pages the merchant's charges, newest first.
func (s *Service) ListRunningCharges(ctx context.Context, params billing.RunningChargeListParams) (*billing.ListPage[billing.RunningCharge], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	page, err := s.moneyService().ListRunningCharges(ctx, params)
	if err != nil {
		return nil, err
	}
	return &page, nil
}
