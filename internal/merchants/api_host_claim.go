package merchants

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

// A merchant proves it controls a domain before the domain routes to it
// (#1107): the claim's random token must appear in a TXT record at
// _openrails-challenge.<host>. The proof is a DNS query to the configured
// resolver only; OpenRails never connects to the claimed host.

// ChallengeLabel is the DNS label a claim's token is published under.
const ChallengeLabel = "_openrails-challenge"

// ChallengeLookupTimeout bounds one proof lookup.
const ChallengeLookupTimeout = 5 * time.Second

// ErrAPIHostUnproven indicates the challenge record does not carry the
// claim's token, or could not be read.
var ErrAPIHostUnproven = errors.New("merchants: api host control is not proven")

// ErrAPIHostClaimMissing indicates the merchant has no open claim to prove.
var ErrAPIHostClaimMissing = errors.New("merchants: no api host claim to verify")

// APIHostClaim is a merchant's unproven api_host: the host and the token to
// publish at Record.
type APIHostClaim struct {
	APIHost   string
	Token     string
	CreatedAt time.Time
}

// Record is the DNS name that must carry the claim's token as a TXT value.
func (c APIHostClaim) Record() string { return ChallengeLabel + "." + c.APIHost }

// ClaimAPIHost opens id's claim on host with a fresh token, replacing any
// earlier claim. A claim routes nothing; VerifyAPIHost binds the host.
func (s *Service) ClaimAPIHost(ctx context.Context, id billing.MerchantID, host string) (*APIHostClaim, error) {
	if id.IsZero() {
		return nil, errors.New("merchants: merchant id is required")
	}
	host = NormalizeAPIHost(host)
	if err := ValidateAPIHost(host); err != nil {
		return nil, err
	}
	if net.ParseIP(host) != nil {
		return nil, fmt.Errorf("%w: %q is an address, not a domain", ErrInvalidAPIHost, host)
	}
	q := gen.New(s.pool)
	taken, err := q.MerchantAPIHostTaken(ctx, gen.MerchantAPIHostTakenParams{ApiHost: host, ID: id.UUID()})
	if err != nil {
		return nil, fmt.Errorf("merchants: claim api host: %w", err)
	}
	if taken {
		return nil, fmt.Errorf("merchants: claim api host %q: %w", host, ErrAPIHostTaken)
	}
	row, err := q.UpsertMerchantAPIHostClaim(ctx, gen.UpsertMerchantAPIHostClaimParams{MerchantID: id.UUID(), ApiHost: host, Token: rand.Text()})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMerchantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("merchants: claim api host: %w", err)
	}
	return &APIHostClaim{APIHost: row.ApiHost, Token: row.Token, CreatedAt: row.CreatedAt}, nil
}

// APIHostClaimOf returns id's open claim, or nil.
func (s *Service) APIHostClaimOf(ctx context.Context, id billing.MerchantID) (*APIHostClaim, error) {
	row, err := gen.New(s.pool).GetMerchantAPIHostClaim(ctx, id.UUID())
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("merchants: read api host claim: %w", err)
	}
	return &APIHostClaim{APIHost: row.ApiHost, Token: row.Token, CreatedAt: row.CreatedAt}, nil
}

// VerifyAPIHost proves id's open claim through resolver (nil: the system
// resolver): a TXT record at the claim's Record must carry its token. Proven,
// the host becomes id's api_host and the claim closes; a host another merchant
// already holds is ErrAPIHostTaken, one reserved for the deployment
// ErrAPIHostReserved.
func (s *Service) VerifyAPIHost(ctx context.Context, id billing.MerchantID, reserved []string, resolver *net.Resolver) (string, error) {
	claim, err := s.APIHostClaimOf(ctx, id)
	if err != nil {
		return "", err
	}
	if claim == nil {
		return "", ErrAPIHostClaimMissing
	}
	if err := ClaimableAPIHost(claim.APIHost, reserved); err != nil {
		return "", err
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	lookup, cancel := context.WithTimeout(ctx, ChallengeLookupTimeout)
	defer cancel()
	values, err := resolver.LookupTXT(lookup, claim.Record())
	if err != nil || !slices.ContainsFunc(values, func(v string) bool { return strings.TrimSpace(v) == claim.Token }) {
		return "", ErrAPIHostUnproven
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The token proven is the one still open: a newer claim waits for its own proof.
		q := gen.New(tx)
		n, err := q.DeleteProvenMerchantAPIHostClaim(ctx, gen.DeleteProvenMerchantAPIHostClaimParams{MerchantID: id.UUID(), ApiHost: claim.APIHost, Token: claim.Token})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrAPIHostClaimMissing
		}
		n, err = q.SetMerchantAPIHost(ctx, gen.SetMerchantAPIHostParams{ID: id.UUID(), ApiHost: claim.APIHost})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrMerchantNotFound
		}
		return nil
	})
	switch {
	case err == nil:
		return claim.APIHost, nil
	case isUniqueViolation(err):
		return "", fmt.Errorf("merchants: verify api host %q: %w", claim.APIHost, ErrAPIHostTaken)
	case errors.Is(err, ErrAPIHostClaimMissing), errors.Is(err, ErrMerchantNotFound):
		return "", err
	default:
		return "", fmt.Errorf("merchants: verify api host: %w", err)
	}
}

// ReleaseAPIHost clears id's api_host and any open claim: giving a host up
// needs no proof.
func (s *Service) ReleaseAPIHost(ctx context.Context, id billing.MerchantID) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.DeleteMerchantAPIHostClaim(ctx, id.UUID()); err != nil {
			return err
		}
		n, err := q.SetMerchantAPIHost(ctx, gen.SetMerchantAPIHostParams{ID: id.UUID()})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrMerchantNotFound
		}
		return nil
	})
}
