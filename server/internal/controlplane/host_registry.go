package controlplane

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchants"
)

// ErrHostMerchantUnknown indicates a request Host maps to no active merchant
// (unregistered or cleared host, inactive merchant, or an ambiguous match).
// Every caller fails closed: there is no fallback merchant.
var ErrHostMerchantUnknown = errors.New("controlplane: host maps to no active merchant")

// merchantForHost resolves the active merchant whose api_host is host, with
// merchantForGroupID's guard (merchantDirectoryRow). It reads live on every call,
// so a merchant re-hosted on any node resolves on the next request everywhere.
func (c *ControlPlane) merchantForHost(ctx context.Context, host string) (billing.MerchantID, string, error) {
	host = merchants.NormalizeAPIHost(host)
	if host == "" {
		return billing.MerchantID{}, "", ErrHostMerchantUnknown
	}
	if c == nil || c.pool == nil {
		return billing.MerchantID{}, "", errors.New("controlplane: pgx pool unavailable for host resolution")
	}
	mid, slug, err := c.merchantDirectoryRow(gen.New(c.pool).ListLiveMerchantsByAPIHost(ctx, host))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrServiceCredentialMerchantUnresolved) {
			return billing.MerchantID{}, "", ErrHostMerchantUnknown
		}
		return billing.MerchantID{}, "", err
	}
	return mid, slug, nil
}

// ResolveMerchantByHost resolves a request Host to its active merchant, for
// Host-routed merchant scope and customer routes. It is an
// internal/merchant.HostResolver.
func (c *ControlPlane) ResolveMerchantByHost(ctx context.Context, host string) (billing.MerchantID, error) {
	mid, _, err := c.merchantForHost(ctx, host)
	return mid, err
}
