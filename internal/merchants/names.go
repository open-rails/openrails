package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchantdocs"
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
type RenameTooSoonError = billing.MerchantRenameTooSoonError

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
		out = append(out, DirectoryRef{ID: billing.MerchantID(row.ID), Slug: row.Slug, DisplayName: s.DisplayName(ctx, billing.MerchantID(row.ID)), GroupID: row.GroupID})
	}
	return out, nil
}

// DisplayName is the display name the merchant's configuration sets; "" when
// it sets none or cannot be read now.
func (s *Service) DisplayName(ctx context.Context, id billing.MerchantID) string {
	if s == nil || s.config == nil {
		return ""
	}
	set, err := s.config.Get(ctx, id)
	if err != nil {
		return ""
	}
	return set.Merchant.Value.DisplayName
}

// SetDisplayName sets the merchant document's display name; "" is a no-op.
// A manifest's merchants take theirs from the manifest.
func (s *Service) SetDisplayName(ctx context.Context, id billing.MerchantID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if s == nil || s.config == nil {
		return errors.New("merchants: merchant configuration unavailable")
	}
	if !s.config.Writable() {
		return ErrConfigReadOnly
	}
	for attempt := 0; ; attempt++ {
		set, err := s.config.Reload(ctx, id)
		if err != nil {
			return err
		}
		doc := set.Merchant.Value
		if doc.DisplayName == name {
			return nil
		}
		doc.DisplayName = name
		_, err = s.config.PutMerchant(ctx, id, doc, set.Merchant.Revision)
		if errors.Is(err, merchantdocs.ErrRevisionMismatch) {
			if attempt >= 2 {
				return ErrRevisionMismatch
			}
			continue
		}
		return err
	}
}

// nameClaimError maps the live-name index and the alias guard to one refusal.
func nameClaimError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		(pgErr.ConstraintName == "merchants_slug_key" || pgErr.ConstraintName == "merchant_slug_aliases_pkey") {
		return fmt.Errorf("%w: %w", billing.ErrMerchantNameTaken, err)
	}
	return err
}

// MerchantSettings is the merchant document's display name and settings: the
// configuration a database handle reads through (db.MerchantConfig).
func (s *Service) MerchantSettings(ctx context.Context, id billing.MerchantID) (string, billing.MerchantSettings, error) {
	if s == nil || s.config == nil {
		return "", billing.MerchantSettings{}, errors.New("merchants: merchant configuration unavailable")
	}
	return s.config.MerchantSettings(ctx, id)
}

// Of is the merchants service bound to d as its merchant configuration; nil
// when none is.
func Of(d *db.DB) *Service {
	s, _ := d.MerchantConfig().(*Service)
	return s
}
