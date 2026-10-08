// Package providerrecovery holds writes against an established provider book
// until its bounded provider observations and financial receipts have caught up.
package providerrecovery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// RefreshInterval is the existing provider refresh cadence. SafetyLag is the
// provider-window delay; neither promises that a provider has indexed all truth.
const RefreshInterval = 4 * time.Hour
const SafetyLag = 5 * time.Minute

var ErrPending = errors.New("provider recovery required")

// CheckPSP is for writes on existing obligations. Initial checkout is not a
// restore operation and does not call this gate. Known receipt verification
// must remain possible while writes are held, so it can settle accepted work.
func CheckPSP(ctx context.Context, database *db.DB, mid, psp uuid.UUID, now time.Time) error {
	if database == nil || mid == uuid.Nil || psp == uuid.Nil {
		return fmt.Errorf("%w: account scope unavailable", ErrPending)
	}
	ctx = merchant.WithID(ctx, billing.MerchantID(mid))
	return database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := database.Gen(ctx)
		floor := now.Add(-RefreshInterval - SafetyLag)
		age, err := q.PSPRecoveryBookAge(ctx, gen.PSPRecoveryBookAgeParams{MerchantID: mid, PspID: psp, Before: floor, Latest: now.Add(SafetyLag)})
		if err != nil {
			return fmt.Errorf("%w: read account history: %v", ErrPending, err)
		}
		if age.Future {
			return fmt.Errorf("%w: PSP %s has future-dated billing evidence", ErrPending, psp)
		}
		applied, err := q.GetPSPAppliedRefreshWatermark(ctx, gen.GetPSPAppliedRefreshWatermarkParams{MerchantID: mid, PspID: psp})
		if err == nil && applied.After(now.Add(SafetyLag)) {
			return fmt.Errorf("%w: PSP %s has future-dated applied coverage", ErrPending, psp)
		}
		if err == nil && !applied.Before(floor) {
			return nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: read applied coverage: %v", ErrPending, err)
		}
		if !age.Established {
			return nil
		}
		return fmt.Errorf("%w: PSP %s needs complete applied provider coverage", ErrPending, psp)
	})
}

// CheckMerchant covers maintenance that can affect any established PSP book.
// An unreadable or stale account holds that maintenance; unrelated accounts
// still perform their own observation and operation-specific recovery.
func CheckMerchant(ctx context.Context, database *db.DB, mid uuid.UUID, now time.Time) error {
	if database == nil || mid == uuid.Nil {
		return fmt.Errorf("%w: merchant scope unavailable", ErrPending)
	}
	ctx = merchant.WithID(ctx, billing.MerchantID(mid))
	return database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		accounts, err := database.Gen(ctx).ListPSPsForMerchant(ctx, mid)
		if err != nil {
			return fmt.Errorf("%w: list accounts: %v", ErrPending, err)
		}
		for _, account := range accounts {
			if err := CheckPSP(ctx, database, mid, account.ID, now); err != nil {
				return err
			}
		}
		return nil
	})
}
