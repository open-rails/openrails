package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Merchant retirement is the core mechanism behind hosted name recycling. Core
// owns only safety: identity binding, reserved names, activity facts, the
// irreversible tombstone and group-release recovery. When and why to retire
// (warning cadence, notice state, arming) is host policy.

const maxRetirementCandidatePage = 500

// GroupReleaser deletes the captured permission group UUID with its slug
// released, returning nil when that group is already gone so recovery converges.
type GroupReleaser func(ctx context.Context, groupID string) error

// ErrGroupReleasePending reports a committed retirement whose group release has
// not completed yet; CompletePendingGroupReleases retries it by UUID.
var ErrGroupReleasePending = errors.New("merchants: retired merchant group release pending")

// RetirementCursor is the keyset position after a candidate.
type RetirementCursor struct {
	CreatedAt  time.Time
	MerchantID merchant.ID
}

// RetirementCandidatesRequest pages live, group-bound, unreserved merchants
// created before CreatedBefore, oldest first.
type RetirementCandidatesRequest struct {
	CreatedBefore time.Time
	After         *RetirementCursor
	// Limit is the page size, 1..500.
	Limit int
}

// RetirementCandidate is one merchant plus its current activity fact.
type RetirementCandidate struct {
	MerchantID merchant.ID
	Slug       string
	GroupID    string
	CreatedAt  time.Time
	// Used reports any retirement-blocking activity (MerchantHasActivity).
	Used bool
}

// RetirementCandidatePage is one keyset page; Next is nil at the end.
type RetirementCandidatePage struct {
	Candidates []RetirementCandidate
	Next       *RetirementCursor
}

// RetirementRefusal names why a merchant was not retired.
type RetirementRefusal string

const (
	RetirementRefusedNotLive       RetirementRefusal = "not_live"
	RetirementRefusedGroupMismatch RetirementRefusal = "group_mismatch"
	RetirementRefusedReserved      RetirementRefusal = "reserved"
	RetirementRefusedActive        RetirementRefusal = "active"
)

// RetireResult reports one retirement attempt. Retired means the tombstone is
// committed; Refusal is set otherwise.
type RetireResult struct {
	Retired bool
	Refusal RetirementRefusal
}

// ListRetirementCandidates returns one page of candidates. Activity is probed
// per merchant inside its own RLS scope.
func (s *Service) ListRetirementCandidates(ctx context.Context, req RetirementCandidatesRequest, reservedSlugs []string) (RetirementCandidatePage, error) {
	var page RetirementCandidatePage
	if s == nil || s.pool == nil {
		return page, errors.New("merchants: retirement candidates require a DB pool")
	}
	if req.CreatedBefore.IsZero() {
		return page, errors.New("merchants: retirement candidates require a creation cutoff")
	}
	if req.Limit <= 0 || req.Limit > maxRetirementCandidatePage {
		return page, fmt.Errorf("merchants: retirement candidate limit %d outside 1..%d", req.Limit, maxRetirementCandidatePage)
	}
	after := RetirementCursor{}
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
		mid := merchant.ID(row.ID)
		var used bool
		if err := s.pool.MerchantTx(ctx, mid, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			used, err = gen.New(tx).MerchantHasActivity(ctx, mid.UUID())
			return err
		}); err != nil {
			return page, fmt.Errorf("merchants: probe activity for %s: %w", mid, err)
		}
		page.Candidates = append(page.Candidates, RetirementCandidate{
			MerchantID: mid, Slug: row.Slug, GroupID: row.GroupID, CreatedAt: row.CreatedAt, Used: used,
		})
	}
	if len(rows) == req.Limit {
		last := page.Candidates[len(page.Candidates)-1]
		page.Next = &RetirementCursor{CreatedAt: last.CreatedAt, MerchantID: last.MerchantID}
	}
	return page, nil
}

// RetireUnused atomically retires a never-used merchant bound to groupID, then
// releases that exact group. The merchant row lock serializes activity inserts
// (every blocker references it), so the activity check and the irreversible
// tombstone commit together. A release failure after commit returns Retired
// with ErrGroupReleasePending.
func (s *Service) RetireUnused(ctx context.Context, mid merchant.ID, groupID string, reservedSlugs []string, release GroupReleaser) (RetireResult, error) {
	var res RetireResult
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
			res.Refusal = RetirementRefusedNotLive
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case !state.Live:
			res.Refusal = RetirementRefusedNotLive
			return nil
		case state.PermissionGroupID == nil || *state.PermissionGroupID != groupID:
			res.Refusal = RetirementRefusedGroupMismatch
			return nil
		case containsSlug(reserved, state.Slug):
			res.Refusal = RetirementRefusedReserved
			return nil
		}
		used, err := q.MerchantHasActivity(ctx, mid.UUID())
		if err != nil {
			return err
		}
		if used {
			res.Refusal = RetirementRefusedActive
			return nil
		}
		if err := q.MarkMerchantRetired(ctx, gen.MarkMerchantRetiredParams{ID: mid.UUID(), RetiredAt: time.Now().UTC()}); err != nil {
			return err
		}
		res.Retired = true
		return nil
	})
	if err != nil {
		return RetireResult{}, fmt.Errorf("merchants: retire %s: %w", mid, err)
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
		if err := s.releaseRetiredGroup(ctx, merchant.ID(item.ID), item.GroupID, release); err != nil {
			errs = append(errs, err)
			continue
		}
		completed++
	}
	return completed, errors.Join(errs...)
}

func (s *Service) releaseRetiredGroup(ctx context.Context, mid merchant.ID, groupID string, release GroupReleaser) error {
	if err := release(ctx, groupID); err != nil {
		return fmt.Errorf("%w: merchant %s group %s: %w", ErrGroupReleasePending, mid, groupID, err)
	}
	return s.pool.MerchantTx(ctx, mid, func(ctx context.Context, tx pgx.Tx) error {
		return gen.New(tx).CompleteMerchantGroupRelease(ctx, gen.CompleteMerchantGroupReleaseParams{ID: mid.UUID(), GroupID: groupID})
	})
}

func normalizeReserved(slugs []string) []string {
	out := make([]string, 0, len(merchant.ReservedHostedSlugs)+len(slugs))
	for _, slug := range append(append([]string{}, merchant.ReservedHostedSlugs...), slugs...) {
		if slug = merchant.NormalizeSlug(slug); slug != "" && !containsSlug(out, slug) {
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
