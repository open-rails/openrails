package merchants

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
)

// OpenRails owns merchant names (#1106). merchants.slug is the name of every
// live merchant; a rename leaves an alias that forwards to the merchant until it
// expires. The database guard (guard_merchant_name) keeps live names and
// unexpired aliases one namespace and releases both when a merchant leaves the
// directory. Every time is the database's now().

// ErrInvalidName reports a name that is not a legal merchant name.
var ErrInvalidName = errors.New("merchants: invalid merchant name")

// ErrRenamesDisabled reports a deployment whose naming policy forbids renames.
var ErrRenamesDisabled = errors.New("merchants: merchant renames are disabled")

// RenameTooSoonError reports a rename before the policy's interval elapsed.
type RenameTooSoonError struct{ NextRenameAt time.Time }

func (e *RenameTooSoonError) Error() string {
	return fmt.Sprintf("merchants: the next rename is allowed at %s", e.NextRenameAt.UTC().Format(time.RFC3339))
}

// Rename gives a live merchant a new name. The former name becomes an alias
// under policy, and a name the merchant itself held before is reclaimed.
func (s *Service) Rename(ctx context.Context, id billing.MerchantID, name string, policy config.NamingPolicy) (*Merchant, error) {
	name = normalizeSlug(name)
	if err := billing.ValidateMerchantSlug(name); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidName, err)
	}
	if !policy.Enabled {
		return nil, ErrRenamesDisabled
	}
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.LockMerchantNameForRename(ctx, id.UUID())
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMerchantNotFound
		}
		if err != nil || row.Slug == name {
			return err
		}
		if row.SlugChangedAt != nil {
			if next := row.SlugChangedAt.Add(policy.RenameInterval); row.Now.Before(next) {
				return &RenameTooSoonError{NextRenameAt: next}
			}
		}
		if err := q.RenameMerchant(ctx, gen.RenameMerchantParams{ID: id.UUID(), Slug: name}); err != nil {
			return err
		}
		if policy.FormerNames == config.FormerNamesImmediate {
			return nil
		}
		return q.InsertMerchantSlugAlias(ctx, gen.InsertMerchantSlugAliasParams{
			Slug: row.Slug, MerchantID: id.UUID(),
			Forever:          policy.FormerNames == config.FormerNamesForever,
			RetentionSeconds: policy.FormerNameRetention.Seconds(),
		})
	})
	if err != nil {
		return nil, nameClaimError(err)
	}
	return s.Get(ctx, id)
}

// GetBySlug resolves a live name or an unexpired former name to its live
// merchant. The returned Slug is always the current name.
func (s *Service) GetBySlug(ctx context.Context, name string) (*Merchant, error) {
	name = normalizeSlug(name)
	row, err := s.database.Gen(ctx).GetMerchantBySlugOrAlias(ctx, name)
	return toMerchant(row.ID, row.Slug, row.Status, row.PermissionGroupID, err)
}

// ListByGroups returns the live merchants bound to the given AuthKit groups,
// ordered by name.
func (s *Service) ListByGroups(ctx context.Context, groupIDs []string) ([]DirectoryRef, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	rows, err := s.database.Gen(ctx).ListLiveMerchantsByGroupIDs(ctx, groupIDs)
	if err != nil {
		return nil, err
	}
	var out []DirectoryRef
	for _, row := range rows {
		out = append(out, DirectoryRef{ID: billing.MerchantID(row.ID), Slug: row.Slug, DisplayName: row.DisplayName, GroupID: row.GroupID})
	}
	return out, nil
}

// nameClaimError maps the live-name index and the alias guard to one refusal.
func nameClaimError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		(pgErr.ConstraintName == "uq_merchants_live_slug" || pgErr.ConstraintName == "merchant_slug_aliases_pkey") {
		return fmt.Errorf("%w: %w", billing.ErrMerchantNameTaken, err)
	}
	return err
}
