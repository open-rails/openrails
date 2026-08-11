package merchants

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"
)

// GroupSlugResolver resolves a merchant slug through the authkit merchant-group
// namespace to the bound permission-group id and the group's CURRENT slug
// (or#914). Implementations follow ak#264 slug tombstones, so a renamed-away
// slug resolves to the same group forever. The control plane provides one
// (ControlPlane.MerchantGroupSlugResolver); a runtime with no control plane
// leaves it nil and slug resolution stays table-only.
type GroupSlugResolver func(ctx context.Context, slug string) (groupID, currentSlug string, err error)

// WithGroupSlugResolver wires the authkit group namespace into directory slug
// resolution (or#914): slugs that miss the openrails.merchants table are
// retried through the group namespace, which follows renames. Nil is a no-op.
func (s *Service) WithGroupSlugResolver(r GroupSlugResolver) *Service {
	if s != nil && r != nil {
		s.groupSlugResolver = r
	}
	return s
}

// merchantByGroupFallback is the shared rename-forwarding miss path (or#914):
// resolve slug -> group (tombstone-following) -> directory row by its group
// binding. On a hit whose stored slug drifted from the group's current slug
// the row is lazily re-synced; the returned Merchant carries the group's
// CURRENT slug (the group is the naming authority). Returns
// ErrMerchantNotFound when the seam is unwired or nothing matches.
func (s *Service) merchantByGroupFallback(ctx context.Context, slug string) (*Merchant, error) {
	if s == nil || s.groupSlugResolver == nil || s.pool == nil {
		return nil, ErrMerchantNotFound
	}
	groupID, current, err := s.groupSlugResolver(ctx, slug)
	if err != nil || strings.TrimSpace(groupID) == "" {
		return nil, ErrMerchantNotFound
	}
	row := s.pool.QueryRow(ctx, `SELECT `+merchantSelectCols+`
		FROM openrails.merchants WHERE permission_group_id = $1 AND deleted_at IS NULL`, groupID)
	m, err := scanMerchant(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMerchantNotFound
		}
		return nil, err
	}
	if current = strings.TrimSpace(current); current != "" && current != m.Slug {
		// Best effort: a failure (e.g. a non-group-bound row coincidentally
		// holding the target slug) leaves the row stale; resolution keeps
		// working through this fallback either way.
		if _, uerr := s.pool.Exec(ctx, `
			UPDATE openrails.merchants
			   SET slug = $2, updated_at = current_timestamp
			 WHERE id = $1::uuid AND deleted_at IS NULL AND slug IS DISTINCT FROM $2
		`, m.ID.String(), current); uerr != nil {
			log.WithError(uerr).WithFields(log.Fields{"merchant_id": m.ID.String(), "slug": current}).
				Warn("merchants: lazy slug re-sync after group rename failed; resolution continues via group forwarding (or#914)")
		}
		m.Slug = current
	}
	return m, nil
}
