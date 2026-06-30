package repo

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// resolvePrimaryProviderAccountID best-effort resolves the merchant's primary
// enabled provider account for a rail (#641), used to stamp new payments /
// subscriptions / payment methods with provider_account_id. Returns nil when no
// primary is configured or on ANY error — stamping is advisory and must never
// fail a money write. Per-account paths (e.g. inbound webhooks for a specific
// account) set provider_account_id explicitly and bypass this fallback.
func resolvePrimaryProviderAccountID(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, rail models.Rail) *uuid.UUID {
	if q == nil || merchantID == uuid.Nil || rail == "" {
		return nil
	}
	pa, err := q.GetPrimaryProviderAccount(ctx, gen.GetPrimaryProviderAccountParams{
		MerchantID:   merchantID,
		ProviderType: string(rail),
	})
	if err != nil {
		return nil
	}
	id := pa.ID
	return &id
}
