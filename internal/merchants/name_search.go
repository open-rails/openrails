package merchants

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

type GroupSearchResolver func(context.Context, string, string, string, int) ([]merchant.GroupName, error)

func (s *Service) WithGroupSearchResolver(search GroupSearchResolver) *Service {
	if s != nil {
		s.groupSearchResolver = search
	}
	return s
}

// SearchMerchants merges canonical bound names with the local unbound namespace.
// Authority pages are continued when some groups have no live billing binding;
// neither a stale projection nor a full-directory N+1 scan answers this lookup.
func (s *Service) SearchMerchants(ctx context.Context, query string, limit int) ([]Merchant, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("merchants: search requires a non-empty query")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := s.database.Gen(ctx)
	local, err := q.SearchUnboundMerchants(ctx, gen.SearchUnboundMerchantsParams{Query: query, PageLimit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Merchant, 0, len(local)+limit)
	for _, row := range local {
		out = append(out, Merchant{ID: merchant.ID(row.ID), Slug: row.Slug, Status: MerchantStatus(row.Status)})
	}
	if s.groupSlugResolver != nil {
		if s.groupSearchResolver == nil {
			return nil, errors.New("merchants: authoritative group search unavailable")
		}
		var afterName, afterID string
		found := 0
		for found < limit {
			page, err := s.groupSearchResolver(ctx, query, afterName, afterID, 200)
			if err != nil {
				return nil, err
			}
			if len(page) == 0 {
				break
			}
			ids := make([]string, 0, len(page))
			for _, group := range page {
				ids = append(ids, group.ID)
			}
			bindings, err := q.ListMerchantBindingsByGroups(ctx, ids)
			if err != nil {
				return nil, err
			}
			byGroup := make(map[string]gen.ListMerchantBindingsByGroupsRow, len(bindings))
			for _, binding := range bindings {
				byGroup[binding.GroupID] = binding
			}
			for _, group := range page {
				if binding, ok := byGroup[group.ID]; ok {
					out = append(out, Merchant{ID: merchant.ID(binding.ID), Slug: group.Name, Status: MerchantStatus(binding.Status), PermissionGroupID: group.ID})
					found++
					if found == limit {
						break
					}
				}
			}
			last := page[len(page)-1]
			if last.Name < afterName || (last.Name == afterName && last.ID <= afterID) {
				return nil, fmt.Errorf("merchants: group search cursor did not advance")
			}
			afterName, afterID = last.Name, last.ID
			if len(page) < 200 {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slug != out[j].Slug {
			return out[i].Slug < out[j].Slug
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
