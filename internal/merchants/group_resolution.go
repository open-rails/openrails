package merchants

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"
)

// GroupSlugResolver resolves a current or active former name to immutable group
// identity and its canonical name. Alias lifetime is owned by AuthKit.
type GroupSlugResolver func(ctx context.Context, slug string) (groupID, currentSlug string, err error)

// WithGroupSlugResolver selects AuthKit as the directory's naming authority.
func (s *Service) WithGroupSlugResolver(r GroupSlugResolver) *Service {
	if s != nil && r != nil {
		s.groupSlugResolver = r
	}
	return s
}

// merchantByGroupName resolves identity before reading the billing directory.
func (s *Service) merchantByGroupName(ctx context.Context, slug string) (*Merchant, error) {
	if s == nil || s.groupSlugResolver == nil || s.pool == nil {
		return nil, ErrMerchantNotFound
	}
	groupID, current, err := s.groupSlugResolver(ctx, slug)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(groupID) == "" {
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
