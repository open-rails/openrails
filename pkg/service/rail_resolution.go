package service

import (
	"context"

	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/pkg/merchant"

	log "github.com/sirupsen/logrus"
)

// railArmed reports whether the ctx merchant's rail is armed (#788 Layer C).
// Resolution errors read as unarmed — every caller refuses to act on false,
// never default-allows.
func (s *Service) railArmed(ctx context.Context, rail string) bool {
	if s == nil || s.rt == nil || s.rt.RailConfigs == nil {
		return false
	}
	armed, err := s.rt.RailConfigs.Armed(ctx, rail)
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("rail", rail).Warn("rail armed-state resolution failed; treating as unarmed (fail closed)")
		return false
	}
	return armed
}

// resolveNMIClientForMerchant arms the ctx merchant's NMI client from the
// armed rail state (#788). nil = not armed / not resolvable (callers skip
// their NMI pass — they never fall back to another credential source).
func (s *Service) resolveNMIClientForMerchant(ctx context.Context) *nmi.NMIClient {
	if s == nil || s.rt == nil || s.rt.CollectionResolver == nil {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil
	}
	client, ok, err := s.rt.CollectionResolver.ResolveNMIClient(ctx, mid.UUID(), nil)
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("nmi client resolution failed; treating as unarmed (fail closed)")
		return nil
	}
	if !ok {
		return nil
	}
	return client
}
