package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Merchant retirement is the core mechanism behind hosted name recycling. Core
// owns only safety: identity binding, reserved names, activity facts, the
// irreversible tombstone and group-release recovery. When and why to retire
// (warning cadence, notice state, arming) is host policy.

const maxRetirementCandidatePage = 500

// GroupReleaser deletes the captured permission group UUID with its slug
// released, returning nil when that group is already gone so recovery converges.
type GroupReleaser func(ctx context.Context, groupID string) error

// ListRetirementCandidates returns one page of candidates. Activity is probed
// per merchant inside its own scope.
func (s *Service) ListRetirementCandidates(ctx context.Context, req billing.MerchantRetirementCandidateListParams, reservedSlugs []string) (billing.MerchantRetirementCandidatePage, error) {
	var page billing.MerchantRetirementCandidatePage
	if s == nil || s.pool == nil {
		return page, errors.New("merchants: retirement candidates require a DB pool")
	}
	if req.CreatedBefore.IsZero() {
		return page, errors.New("merchants: retirement candidates require a creation cutoff")
	}
	if req.Limit <= 0 || req.Limit > maxRetirementCandidatePage {
		return page, fmt.Errorf("merchants: retirement candidate limit %d outside 1..%d", req.Limit, maxRetirementCandidatePage)
	}
	after := billing.MerchantRetirementCursor{}
	if req.After != nil {
		after = *req.After
	}
	rows, err := gen.New(s.pool).ListMerchantRetirementCandidates(ctx, gen.ListMerchantRetirementCandidatesParams{
		CreatedBefore:  req.CreatedBefore,
		ReservedSlugs:  normalizeReserved(reservedSlugs),
		AfterCreatedAt: after.CreatedAt,
		AfterID:        after.MerchantID.UUID(),
		PageLimit:      int64(req.Limit),
	})
	if err != nil {
		return page, fmt.Errorf("merchants: list retirement candidates: %w", err)
	}
	for _, row := range rows {
		mid := billing.MerchantID(row.ID)
		var used bool
		if err := s.pool.MerchantTx(ctx, mid, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			used, err = gen.New(tx).MerchantHasActivity(ctx, mid.UUID())
			return err
		}); err != nil {
			return page, fmt.Errorf("merchants: probe activity for %s: %w", mid, err)
		}
		page.Candidates = append(page.Candidates, billing.MerchantRetirementCandidate{
			MerchantID: mid, Slug: row.Slug, GroupID: row.GroupID, CreatedAt: row.CreatedAt, Used: used,
		})
	}
	if len(rows) == req.Limit {
		last := page.Candidates[len(page.Candidates)-1]
		page.Next = &billing.MerchantRetirementCursor{CreatedAt: last.CreatedAt, MerchantID: last.MerchantID}
	}
	return page, nil
}

// RetireUnused atomically retires a never-used merchant bound to groupID, then
// releases that exact group. The merchant row lock serializes activity inserts
// (every blocker references it), so the activity check and the irreversible
// tombstone commit together. Any failure after that commit returns Retired
// with ErrGroupReleasePending.
func (s *Service) RetireUnused(ctx context.Context, mid billing.MerchantID, groupID string, reservedSlugs []string, release GroupReleaser) (billing.MerchantRetirement, error) {
	var res billing.MerchantRetirement
	if s == nil || s.pool == nil {
		return res, errors.New("merchants: retirement requires a DB pool")
	}
	groupID = strings.TrimSpace(groupID)
	if mid.IsZero() || groupID == "" {
		return res, errors.New("merchants: retirement requires a merchant and its expected group")
	}
	if release == nil {
		return res, errors.New("merchants: retirement requires a group releaser")
	}
	reserved := normalizeReserved(reservedSlugs)
	err := s.pool.MerchantTx(ctx, mid, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		state, err := q.LockMerchantRetirementState(ctx, mid.UUID())
		if errors.Is(err, pgx.ErrNoRows) {
			res.Refusal = billing.MerchantRetirementRefusedNotLive
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case !state.Live:
			res.Refusal = billing.MerchantRetirementRefusedNotLive
			return nil
		case state.PermissionGroupID == nil || *state.PermissionGroupID != groupID:
			res.Refusal = billing.MerchantRetirementRefusedGroupMismatch
			return nil
		case containsSlug(reserved, state.Slug):
			res.Refusal = billing.MerchantRetirementRefusedReserved
			return nil
		}
		used, err := q.MerchantHasActivity(ctx, mid.UUID())
		if err != nil {
			return err
		}
		if used {
			res.Refusal = billing.MerchantRetirementRefusedActive
			return nil
		}
		if err := q.MarkMerchantRetired(ctx, gen.MarkMerchantRetiredParams{ID: mid.UUID(), RetiredAt: time.Now().UTC()}); err != nil {
			return err
		}
		res.Retired = true
		return nil
	})
	if err != nil {
		return billing.MerchantRetirement{}, fmt.Errorf("merchants: retire %s: %w", mid, err)
	}
	if !res.Retired {
		return res, nil
	}
	return res, s.releaseRetiredGroup(ctx, mid, groupID, release)
}

// CompletePendingGroupReleases retries committed retirements whose group
// release did not complete, always by the captured group UUID.
func (s *Service) CompletePendingGroupReleases(ctx context.Context, limit int, release GroupReleaser) (int, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("merchants: retirement recovery requires a DB pool")
	}
	if release == nil {
		return 0, errors.New("merchants: retirement recovery requires a group releaser")
	}
	if limit <= 0 || limit > maxRetirementCandidatePage {
		return 0, fmt.Errorf("merchants: retirement recovery limit %d outside 1..%d", limit, maxRetirementCandidatePage)
	}
	items, err := gen.New(s.pool).ListPendingMerchantGroupReleases(ctx, int64(limit))
	if err != nil {
		return 0, err
	}
	var errs []error
	completed := 0
	for _, item := range items {
		if err := s.releaseRetiredGroup(ctx, billing.MerchantID(item.ID), item.GroupID, release); err != nil {
			errs = append(errs, err)
			continue
		}
		completed++
	}
	return completed, errors.Join(errs...)
}

func (s *Service) releaseRetiredGroup(ctx context.Context, mid billing.MerchantID, groupID string, release GroupReleaser) error {
	if err := release(ctx, groupID); err != nil {
		return fmt.Errorf("%w: merchant %s group %s: %w", billing.ErrMerchantGroupReleasePending, mid, groupID, err)
	}
	if err := s.pool.MerchantTx(ctx, mid, func(ctx context.Context, tx pgx.Tx) error {
		return gen.New(tx).CompleteMerchantGroupRelease(ctx, gen.CompleteMerchantGroupReleaseParams{ID: mid.UUID(), GroupID: groupID})
	}); err != nil {
		return fmt.Errorf("%w: merchant %s group %s released but not recorded: %w", billing.ErrMerchantGroupReleasePending, mid, groupID, err)
	}
	return nil
}

func normalizeReserved(slugs []string) []string {
	out := make([]string, 0, len(billing.ReservedMerchantSlugs)+len(slugs))
	for _, slug := range append(append([]string{}, billing.ReservedMerchantSlugs...), slugs...) {
		if slug = billing.NormalizeMerchantSlug(slug); slug != "" && !containsSlug(out, slug) {
			out = append(out, slug)
		}
	}
	return out
}

func containsSlug(slugs []string, slug string) bool {
	for _, candidate := range slugs {
		if candidate == slug {
			return true
		}
	}
	return false
}
