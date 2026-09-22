package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
)

func catalogOwnerRequest(ctx context.Context) (bool, error) {
	if err := catalog.ValidateOwnerScope(ctx); err != nil {
		return false, err
	}
	_, owned := catalogscope.FromContext(ctx)
	return owned, nil
}

// Creator prices project to the merchant's declared active accounts in the
// current credential environment. Checkout still applies merchant routing; a
// creator cannot choose accounts or supply provider-native identifiers.
func (s *Service) creatorProviderKeys(ctx context.Context) ([]string, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if s.rt == nil || s.rt.DB == nil {
		return nil, fmt.Errorf("merchant database is required for creator collection policy")
	}
	environment := s.catalogProviderEnvironment()
	rows, err := s.catalogDatabase().Gen(ctx).ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{MerchantID: mid.UUID()})
	if err != nil {
		return nil, fmt.Errorf("load creator collection policy: %w", err)
	}
	keys := make(map[string]struct{})
	for _, row := range rows {
		if row.Archived || row.Environment != environment || row.Key == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(*row.Key))
		if key != "" {
			keys[key] = struct{}{}
		}
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}
