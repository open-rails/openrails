package merchants

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrAPIHostTaken indicates apiHost is already assigned to a different active
// merchant (#734: api_host is globally unique).
var ErrAPIHostTaken = errors.New("merchants: api host already assigned to another merchant")

// ErrAPIHostReserved indicates a host the deployment itself serves (its public
// API, console or issuer): a merchant claiming it would take every other
// merchant's traffic on that host.
var ErrAPIHostReserved = errors.New("merchants: api host is reserved for the deployment")

// DeploymentHost is the normalized hostname of one of the deployment's own
// URLs, "" when rawURL names none.
func DeploymentHost(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return NormalizeAPIHost(u.Hostname())
}

// ClaimableAPIHost refuses a normalized host the deployment serves. A
// merchant's own configuration checks it; operators bind hosts freely.
func ClaimableAPIHost(host string, reserved []string) error {
	if host != "" && slices.Contains(reserved, host) {
		return fmt.Errorf("%w: %q", ErrAPIHostReserved, host)
	}
	return nil
}

// ErrInvalidAPIHost indicates a host that is not a plausible DNS hostname
// (#850: api_host is operator-supplied, so the format is enforced here).
var ErrInvalidAPIHost = errors.New("merchants: api host must be a bare lowercase hostname (no scheme, port, or path)")

// NormalizeAPIHost canonicalizes a Host-header value for #734 resolution:
// lowercased, whitespace-trimmed, and stripped of a NUMERIC ":port" suffix
// (Go's net/http leaves the port on Request.Host for non-default ports, and
// local-dev deployments commonly run on a non-443/80 port). Only a real port
// is stripped — SplitHostPort accepts any junk after the colon, and stripping
// e.g. "https://x" down to its "https" prefix would launder a scheme'd
// operator input into a valid-looking label (#850). Empty in, empty out.
func NormalizeAPIHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if h, p, err := net.SplitHostPort(host); err == nil && isNumericPort(p) {
		host = h
	}
	return strings.ToLower(host)
}

func isNumericPort(p string) bool {
	if p == "" {
		return false
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ValidateAPIHost checks a NORMALIZED api_host (NormalizeAPIHost output) is a
// plausible DNS hostname: dot-separated labels of [a-z0-9-], 1-63 chars each,
// no leading/trailing hyphen, 253 chars total. Empty is invalid — callers
// treat empty as "clear the mapping" before validating.
func ValidateAPIHost(host string) error {
	if host == "" || len(host) > 253 {
		return fmt.Errorf("%w: %q", ErrInvalidAPIHost, host)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("%w: %q", ErrInvalidAPIHost, host)
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return fmt.Errorf("%w: %q", ErrInvalidAPIHost, host)
			}
		}
	}
	return nil
}

// SetHostConfig sets a merchant's canonical API host (#734): the per-merchant
// Host->webhook-routing config, engine-owned. apiHost empty clears the
// mapping (the merchant no longer resolves from any Host). This is a plain
// UPDATE on the live directory row: the very next request against the new
// Host resolves immediately, on every process sharing this database (#734's
// multi-node requirement) — there is no boot-time host map to refresh.
// Browser CORS is NOT configured here (#765: it's a static per-route-tier
// policy, not per-merchant — see internal/http/middleware.PermissiveCORSHTTP).
func (s *Service) SetHostConfig(ctx context.Context, id merchant.ID, apiHost string) error {
	if id.IsZero() {
		return errors.New("merchants: merchant id is required")
	}
	host := NormalizeAPIHost(apiHost)
	if host != "" {
		if err := ValidateAPIHost(host); err != nil {
			return err
		}
	}

	n, err := gen.New(s.pool).SetMerchantAPIHost(ctx, gen.SetMerchantAPIHostParams{ID: id.UUID(), ApiHost: host})
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("merchants: set host config for %s host %q: %w", id, host, ErrAPIHostTaken)
		}
		return fmt.Errorf("merchants: set host config: %w", err)
	}
	if n == 0 {
		return ErrMerchantNotFound
	}
	return nil
}

// HostConfig is a merchant's #734 Host configuration.
type HostConfig struct {
	APIHost string
}

// GetHostConfig returns id's current #734 Host configuration.
func (s *Service) GetHostConfig(ctx context.Context, id merchant.ID) (*HostConfig, error) {
	if id.IsZero() {
		return nil, errors.New("merchants: merchant id is required")
	}
	row, err := gen.New(s.pool).GetMerchantDirectoryByID(ctx, id.UUID())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMerchantNotFound
		}
		return nil, fmt.Errorf("merchants: get host config: %w", err)
	}
	cfg := &HostConfig{}
	if row.ApiHost != nil {
		cfg.APIHost = *row.ApiHost
	}
	return cfg, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
