package merchants

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

func (s *Service) retireUnusedMerchant(ctx context.Context, mid merchant.ID, groupID string, now time.Time, warningLead time.Duration) (bool, error) {
	retired := false
	err := s.pool.MerchantTx(ctx, mid, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		live, err := q.LockMerchantRetirementState(ctx, gen.LockMerchantRetirementStateParams{ID: mid.UUID(), GroupID: groupID})
		if err != nil {
			return err
		}
		if !live {
			return nil
		}
		var used bool
		if err := tx.QueryRow(ctx, dormancyUsedProbeSQL, mid.String()).Scan(&used); err != nil {
			return err
		}
		if used {
			return nil
		}
		warned, err := q.LockMerchantRetirementWarning(ctx, mid.UUID())
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if now.Sub(warned) < warningLead {
			return nil
		}
		err = q.MarkMerchantRetired(ctx, gen.MarkMerchantRetiredParams{ID: mid.UUID(), RetiredAt: now})
		retired = err == nil
		return err
	})
	return retired, err
}

func (s *Service) releaseRetiredGroup(ctx context.Context, mid merchant.ID, groupID string, release GroupReleaser) error {
	if err := release(ctx, groupID); err != nil {
		return fmt.Errorf("release retired merchant %s group %s: %w", mid, groupID, err)
	}
	return s.pool.MerchantTx(ctx, mid, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.CompleteMerchantGroupRelease(ctx, gen.CompleteMerchantGroupReleaseParams{ID: mid.UUID(), GroupID: groupID}); err != nil {
			return err
		}
		return q.DeleteMerchantRetirementWarning(ctx, mid.UUID())
	})
}

func (s *Service) resumeGroupRetirements(ctx context.Context, batch int, release GroupReleaser) (int, error) {
	items, err := gen.New(s.pool).ListPendingMerchantGroupReleases(ctx, int64(batch))
	if err != nil {
		return 0, err
	}
	var first error
	completed := 0
	for _, item := range items {
		if err := s.releaseRetiredGroup(ctx, merchant.ID(item.ID), item.GroupID, release); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		completed++
	}
	return completed, first
}
