package merchants

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/pkg/merchant"
)

// OpenRails owns merchant names (#1106). merchants.slug is the name of every
// live merchant; a rename leaves an alias that forwards to the merchant until it
// expires. The database guard (guard_merchant_name) keeps live names and
// unexpired aliases one namespace and releases both when a merchant leaves the
// directory. Every time is the database's now().

// ErrMerchantNameTaken reports a name held by another live merchant or by
// another merchant's unexpired former name.
var ErrMerchantNameTaken = errors.New("merchants: merchant name is taken")

// ErrInvalidName reports a name that is not a legal merchant name.
var ErrInvalidName = errors.New("merchants: invalid merchant name")

// ErrRenamesDisabled reports a deployment whose naming policy forbids renames.
var ErrRenamesDisabled = errors.New("merchants: merchant renames are disabled")

// RenameTooSoonError reports a rename before the policy's interval elapsed.
type RenameTooSoonError struct{ NextRenameAt time.Time }

func (e *RenameTooSoonError) Error() string {
	return fmt.Sprintf("merchants: the next rename is allowed at %s", e.NextRenameAt.UTC().Format(time.RFC3339))
}

// FormerNames selects how long a former name keeps forwarding to its merchant.
type FormerNames string

const (
	FormerNamesFinite    FormerNames = "finite"
	FormerNamesForever   FormerNames = "forever"
	FormerNamesImmediate FormerNames = "immediate"
)

// NamingPolicy governs merchant renames.
type NamingPolicy struct {
	Enabled        bool
	RenameInterval time.Duration
	FormerNames    FormerNames
	// FormerNameRetention is the alias lifetime under FormerNamesFinite.
	FormerNameRetention time.Duration
}

// Rename gives a live merchant a new name. The former name becomes an alias
// under policy, and a name the merchant itself held before is reclaimed.
func (s *Service) Rename(ctx context.Context, id merchant.ID, name string, policy NamingPolicy) (*Merchant, error) {
	name = normalizeSlug(name)
	if err := merchant.ValidateSlug(name); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidName, err)
	}
	if !policy.Enabled {
		return nil, ErrRenamesDisabled
	}
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var current string
		var next *time.Time
		var now time.Time
		err := tx.QueryRow(ctx, `
			SELECT slug, slug_changed_at + make_interval(secs => $2), now()
			  FROM openrails.merchants
			 WHERE id = $1 AND deleted_at IS NULL
			   FOR UPDATE`, id.UUID(), policy.RenameInterval.Seconds()).Scan(&current, &next, &now)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMerchantNotFound
		}
		if err != nil || current == name {
			return err
		}
		if next != nil && now.Before(*next) {
			return &RenameTooSoonError{NextRenameAt: *next}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE openrails.merchants SET slug = $2, slug_changed_at = now(), updated_at = now()
			 WHERE id = $1`, id.UUID(), name); err != nil {
			return err
		}
		if policy.FormerNames == FormerNamesImmediate {
			return nil
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO openrails.merchant_slug_aliases (slug, merchant_id, expires_at)
			VALUES ($1, $2, CASE WHEN $3 THEN NULL ELSE now() + make_interval(secs => $4) END)`,
			current, id.UUID(), policy.FormerNames == FormerNamesForever, policy.FormerNameRetention.Seconds())
		return err
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
	return scanMerchant(s.database.Qx(ctx).QueryRow(ctx, `
		SELECT m.id::text, m.slug, m.status, COALESCE(m.permission_group_id, '')
		  FROM openrails.merchants m
		 WHERE m.slug = $1 AND m.deleted_at IS NULL
		UNION ALL
		SELECT m.id::text, m.slug, m.status, COALESCE(m.permission_group_id, '')
		  FROM openrails.merchant_slug_aliases a
		  JOIN openrails.merchants m ON m.id = a.merchant_id
		 WHERE a.slug = $1 AND (a.expires_at IS NULL OR a.expires_at > now()) AND m.deleted_at IS NULL
		 LIMIT 1`, name))
}

// ListByGroups returns the live merchants bound to the given AuthKit groups,
// ordered by name.
func (s *Service) ListByGroups(ctx context.Context, groupIDs []string) ([]Merchant, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	rows, err := s.database.Qx(ctx).Query(ctx, `
		SELECT id::text, slug, status, permission_group_id
		  FROM openrails.merchants
		 WHERE permission_group_id = ANY($1) AND deleted_at IS NULL
		 ORDER BY slug`, groupIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Merchant
	for rows.Next() {
		m, err := scanMerchant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// nameClaimError maps the live-name index and the alias guard to one refusal.
func nameClaimError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		(pgErr.ConstraintName == "uq_merchants_live_slug" || pgErr.ConstraintName == "merchant_slug_aliases_pkey") {
		return fmt.Errorf("%w: %w", ErrMerchantNameTaken, err)
	}
	return err
}
