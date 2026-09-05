package merchants

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/pkg/merchant"
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
	row := s.database.Qx(ctx).QueryRow(ctx, `SELECT `+merchantSelectCols+`
		FROM openrails.merchants WHERE permission_group_id = $1 AND deleted_at IS NULL`, groupID)
	m, err := scanMerchant(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMerchantNotFound
		}
		return nil, err
	}
	if current = strings.TrimSpace(current); current == "" {
		return nil, ErrMerchantNotFound
	}
	// Project the authoritative name without mutating billing on a read.
	m.Slug = current
	return m, nil
}

// GroupIDResolver reads the current name of one captured immutable group.
type GroupIDResolver func(context.Context, string) (string, error)

func (s *Service) WithGroupIDResolver(r GroupIDResolver) *Service {
	if s != nil {
		s.groupIDResolver = r
	}
	return s
}

// CanonicalSlug projects a merchant identity's current public name. Bound
// merchants require AuthKit; an unbound host-owned row uses its local name.
func (s *Service) CanonicalSlug(ctx context.Context, id merchant.ID) (string, error) {
	m, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if m.PermissionGroupID == "" {
		return m.Slug, nil
	}
	if s.groupIDResolver == nil {
		return "", errors.New("merchants: canonical group resolver unavailable")
	}
	return s.groupIDResolver(ctx, m.PermissionGroupID)
}

// WithNameAuthority configures both name and immutable-group projections together.
func (s *Service) WithNameAuthority(authority merchant.NameAuthority) *Service {
	if s != nil && authority != nil {
		s.WithGroupSlugResolver(authority.ResolveGroup)
		s.WithGroupIDResolver(authority.GroupName)
		s.groupSearchResolver = nil
		if search, ok := authority.(merchant.GroupNameSearch); ok {
			s.WithGroupSearchResolver(search.SearchGroups)
		}
	}
	return s
}
