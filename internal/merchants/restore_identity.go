package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

// ErrMerchantRestoreConflict refuses a restore destination whose UUID, name or
// authority is already assigned differently. Existing rows are never rebound.
var ErrMerchantRestoreConflict = errors.New("merchants: restore destination identity conflict")

// ProvisionForRestore attaches a preserved billing UUID to a destination group
// already authorized by the caller. Ordinary Provision keeps allocating UUIDs.
// This creates only directory identity; the archive importer checks book emptiness.
func (s *Service) ProvisionForRestore(ctx context.Context, id billing.MerchantID, req ProvisionRequest) (*Merchant, bool, error) {
	groupID := strings.TrimSpace(req.PermissionGroupID)
	if groupID == "" {
		return nil, false, ErrPermissionGroupRequired
	}
	return s.restoreIdentity(ctx, id, req.Slug, groupID)
}

// RegisterForRestore creates an explicitly unbound, host-owned destination.
// It never adopts a group-bound row, even if its UUID and display name match.
func (s *Service) RegisterForRestore(ctx context.Context, id billing.MerchantID, slug string) (*Merchant, bool, error) {
	return s.restoreIdentity(ctx, id, slug, "")
}

func (s *Service) restoreIdentity(ctx context.Context, id billing.MerchantID, slug, groupID string) (*Merchant, bool, error) {
	if id.IsZero() {
		return nil, false, fmt.Errorf("merchants: restore merchant_id is required")
	}
	slug = normalizeSlug(slug)
	if err := billing.ValidateMerchantSlug(slug); err != nil {
		return nil, false, err
	}
	if s == nil || s.pool == nil {
		return nil, false, fmt.Errorf("merchants: restore destination requires a directory database")
	}

	// The UUID, group and live-name unique indexes arbitrate concurrent
	// provisions; the name guard refuses another merchant's former name. Never
	// UPDATE on conflict: a UUID is not permission to replace another binding,
	// resurrect a retired row, or rename an existing destination.
	q := gen.New(s.pool)
	_, err := q.InsertRestoredMerchant(ctx, gen.InsertRestoredMerchantParams{ID: id.UUID(), Slug: slug, GroupID: groupID})
	if isUniqueViolation(err) {
		return nil, false, ErrMerchantRestoreConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("merchants: create restore destination: %w", err)
	}
	created := err == nil
	row, err := q.GetUnretiredLiveMerchant(ctx, id.UUID())
	m, err := toMerchant(row.ID, row.Slug, row.Status, row.PermissionGroupID, err)
	if errors.Is(err, ErrMerchantNotFound) {
		return nil, false, ErrMerchantRestoreConflict
	}
	if err != nil {
		return nil, false, fmt.Errorf("merchants: read restore destination: %w", err)
	}
	if m.Status != StatusActive || m.Slug != slug || m.PermissionGroupID != groupID {
		return nil, false, ErrMerchantRestoreConflict
	}
	return m, created, nil
}
