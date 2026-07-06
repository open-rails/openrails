package controlplane

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrHostMerchantUnknown indicates a request Host does not map to any active
// merchant (#734): unregistered host, a merchant whose host mapping was
// cleared, an inactive/deleted merchant, or (defensively) an ambiguous match.
// Every caller fails closed on this error — there is no fallback merchant.
var ErrHostMerchantUnknown = errors.New("controlplane: host maps to no active merchant")

// merchantForHost resolves the merchant whose openrails.merchants.api_host
// equals host, sharing merchantDirectoryRow with merchantForIssuer (#734):
// Host resolution is deliberately the SAME openrails.merchants lookup —
// same active/deleted filtering, same "ambiguous match" guard — issuer
// resolution uses, not a parallel authority. Resolved LIVE per call: no
// boot-time host map exists, so a merchant registered (or re-hosted) on any
// node resolves on the very next request against every process sharing this
// database.
func (c *ControlPlane) merchantForHost(ctx context.Context, host string) (merchant.ID, string, error) {
	host = merchants.NormalizeAPIHost(host)
	if host == "" {
		return merchant.ID{}, "", ErrHostMerchantUnknown
	}
	if c == nil || c.pool == nil {
		return merchant.ID{}, "", errors.New("controlplane: pgx pool unavailable for host resolution")
	}
	mid, slug, err := c.merchantDirectoryRow(ctx, `api_host = $1`, host)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrServiceCredentialMerchantUnresolved) {
			return merchant.ID{}, "", ErrHostMerchantUnknown
		}
		return merchant.ID{}, "", err
	}
	return mid, slug, nil
}

// ResolveMerchantByHost is the exported #734 Host->merchant resolver: the
// single mechanism both merchant-scoped Host resolution and the Host-routed
// webhook mount (pkg/embedded) share. Its signature matches
// pkg/merchant.HostResolver, so a method value (cp.ResolveMerchantByHost) is
// directly assignable wherever that type is expected. Browser CORS no longer
// resolves through this (#765: CORS is now a static per-route-tier policy,
// not per-merchant — see internal/http/middleware.PermissiveCORSHTTP).
func (c *ControlPlane) ResolveMerchantByHost(ctx context.Context, host string) (merchant.ID, error) {
	mid, _, err := c.merchantForHost(ctx, host)
	return mid, err
}
