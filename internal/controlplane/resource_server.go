package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
)

// resourceServer verifies the RFC 9068 access tokens trusted issuers mint
// for this deployment (#1140).
type resourceServer struct {
	verifier *verify.Verifier
	issuers  map[string]trustedIssuer
	origins  map[string]bool
}

type trustedIssuer struct {
	name       string
	merchants  []string
	ceiling    []string
	groupRoles map[string][]string
}

// WithResourceServer accepts the access tokens cfg's trusted issuers mint
// for this deployment. The standalone binary and hosted products set it
// through Config.ControlPlane.ResourceServer.
func WithResourceServer(cfg config.ResourceServerConfig) Option {
	return func(o *options) { o.resourceServer = &cfg }
}

// newResourceServer builds the verifier: one audience (the identifier), DPoP
// with replay protection every replica shares and server nonces.
func newResourceServer(cfg config.ResourceServerConfig, auth *config.AuthConfig, rdb *redis.Client) (*resourceServer, error) {
	if err := config.ValidateResourceServer(&cfg, auth.AllowLoopbackHTTP); err != nil {
		return nil, fmt.Errorf("controlplane: %w", err)
	}
	replay, err := proofReplay(rdb, auth.AllowMemory)
	if err != nil {
		return nil, err
	}
	opts := []verify.VerifierOption{verify.WithDPoP(replay), verify.WithDPoPNonce([]byte(cfg.DPoPNonceKey))}
	if origin := dpopOrigin(auth); origin != "" {
		opts = append(opts, verify.WithPublicURL(origin))
	}
	rs := &resourceServer{verifier: verify.NewVerifier(opts...), issuers: map[string]trustedIssuer{}, origins: map[string]bool{}}
	identifier := strings.TrimSpace(cfg.Identifier)
	for _, is := range cfg.TrustedIssuers {
		issuer := strings.TrimSpace(is.Issuer)
		keys := verify.IssuerOptions{Keys: is.Keys}
		if len(is.Keys) == 0 {
			keys = verify.IssuerOptions{JWKSURI: strings.TrimSpace(is.JWKSURI)}
			if keys.JWKSURI == "" {
				keys.JWKSURI = strings.TrimRight(issuer, "/") + iam.JWKSPath
			}
		}
		if err := rs.verifier.AddIssuer(issuer, []string{identifier}, keys); err != nil {
			return nil, fmt.Errorf("controlplane: trusted issuer %q: %w", issuer, err)
		}
		roles := map[string][]string{}
		for role, merchantRole := range is.GroupRoles {
			grants, ok := merchantRoleGrants(strings.TrimSpace(merchantRole))
			if !ok {
				return nil, fmt.Errorf("controlplane: trusted issuer %q: group role %q maps to %q, not a merchant role", issuer, role, merchantRole)
			}
			roles[strings.TrimSpace(role)] = grants
		}
		rs.issuers[issuer] = trustedIssuer{name: is.Name, merchants: is.Merchants, ceiling: is.Permissions, groupRoles: roles}
		for _, origin := range is.AllowedOrigins {
			rs.origins[strings.TrimRight(strings.TrimSpace(origin), "/")] = true
		}
	}
	return rs, nil
}

// dpopOrigin is where clients reach this deployment, the URL a DPoP proof
// signs: auth.request_origin, else the issuer's origin. Never a request header.
func dpopOrigin(auth *config.AuthConfig) string {
	if origin := strings.TrimRight(strings.TrimSpace(auth.RequestOrigin), "/"); origin != "" {
		return origin
	}
	issuer := strings.TrimSpace(auth.Issuer)
	if i := strings.Index(issuer, "://"); i >= 0 {
		if j := strings.IndexByte(issuer[i+3:], '/'); j >= 0 {
			return issuer[:i+3+j]
		}
	}
	return strings.TrimRight(issuer, "/")
}

// proofReplay claims a DPoP proof once across replicas (Redis), or within the
// one process a memory deployment declared. Its errors fail closed.
func proofReplay(rdb *redis.Client, allowMemory bool) (func(context.Context, string, time.Duration) (bool, error), error) {
	if rdb != nil {
		return func(ctx context.Context, key string, ttl time.Duration) (bool, error) {
			return rdb.SetNX(ctx, "openrails:dpop:"+key, 1, ttl).Result()
		}, nil
	}
	if !allowMemory {
		return nil, errors.New("controlplane: the resource server's DPoP replay protection needs Redis (shared by replicas); set auth.allow_memory=true only for a single-process deployment")
	}
	var mu sync.Mutex
	seen := map[string]time.Time{}
	return func(_ context.Context, key string, ttl time.Duration) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		for k, until := range seen {
			if now.After(until) {
				delete(seen, k)
			}
		}
		if _, used := seen[key]; used {
			return false, nil
		}
		seen[key] = now.Add(ttl)
		return true, nil
	}, nil
}

// AllowedOrigin reports whether origin may call the merchant API across
// origins with a trusted issuer's tokens.
func (c *ControlPlane) AllowedOrigin(origin string) bool {
	return c != nil && c.resource != nil && c.resource.origins[strings.TrimRight(strings.TrimSpace(origin), "/")]
}

// ResolveResourceToken verifies r's RFC 9068 access token and resolves who
// acts, for which merchant and with what permissions:
// (token permissions ∪ the issuer's group roles) ∩ the issuer's ceiling, on
// one of the merchants the issuer is trusted for. The merchant is the
// request's (selector, Host or deployment), or the issuer's only one.
func (c *ControlPlane) ResolveResourceToken(r *http.Request) (*credential.ResolvedResourceAccess, error) {
	if c == nil || c.resource == nil {
		return nil, credential.ErrResourceServerNotConfigured
	}
	if r == nil {
		return nil, credential.ErrResourceTokenInvalid
	}
	ctx := r.Context()
	rs := c.resource
	is, ok := rs.issuers[unverifiedIssuer(r)]
	if !ok {
		return nil, credential.ErrResourceTokenIssuerUnknown
	}
	cl, err := requestauth.Once(ctx, rs.verifier, func() (verify.Claims, error) { return rs.verifier.VerifyRequest(r) })
	if err != nil {
		return nil, resourceTokenError(r, err)
	}
	if !cl.IsResourceToken() || strings.TrimSpace(cl.Subject) == "" {
		return nil, credential.ErrResourceTokenInvalid
	}
	mid, slug, err := c.resourceMerchant(ctx, r, is)
	if err != nil {
		return nil, err
	}
	grants := append([]string(nil), cl.Permissions...)
	for _, role := range cl.Roles {
		grants = append(grants, is.groupRoles[strings.TrimSpace(role)]...)
	}
	return &credential.ResolvedResourceAccess{
		Machine:       cl.Kind == iam.ActorOAuthClient,
		Issuer:        strings.TrimSpace(cl.Issuer),
		Subject:       strings.TrimSpace(cl.Subject),
		ClientID:      cl.ClientID,
		MerchantID:    mid,
		MerchantSlug:  slug,
		Permissions:   credential.IntersectPermissions(grants, is.ceiling),
		Scopes:        append([]string(nil), cl.Scopes...),
		SessionID:     cl.SessionID,
		Email:         cl.Email,
		EmailVerified: cl.EmailVerified,
		Username:      cl.Username,
	}, nil
}

// resourceMerchant is the merchant the token acts for: the one the request
// names, which must be one the issuer is trusted for, else the issuer's only one.
func (c *ControlPlane) resourceMerchant(ctx context.Context, r *http.Request, is trustedIssuer) (billing.MerchantID, string, error) {
	type bound struct {
		id   billing.MerchantID
		slug string
	}
	var trusted []bound
	for _, ref := range is.merchants {
		mid, slug, err := c.MerchantScope(ctx, ref)
		if errors.Is(err, ErrServiceCredentialMerchantUnresolved) {
			continue // not provisioned (yet), or not active
		}
		if err != nil {
			return billing.MerchantID{}, "", fmt.Errorf("%w: %w", credential.ErrResourceTokenUnavailable, err)
		}
		trusted = append(trusted, bound{mid, slug})
	}
	requested, named, err := requestedMerchant(ctx, r, c)
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	if named {
		for _, b := range trusted {
			if b.id == requested {
				return b.id, b.slug, nil
			}
		}
		return billing.MerchantID{}, "", credential.ErrResourceTokenMerchantNotBound
	}
	switch len(trusted) {
	case 1:
		return trusted[0].id, trusted[0].slug, nil
	case 0:
		return billing.MerchantID{}, "", credential.ErrResourceTokenMerchantNotBound
	}
	return billing.MerchantID{}, "", billing.ErrMerchantUnresolved
}

// requestedMerchant is the merchant the request resolved before
// authentication: its selector, Host or the deployment's own.
func requestedMerchant(ctx context.Context, r *http.Request, c *ControlPlane) (billing.MerchantID, bool, error) {
	if target, ok := merchanttarget.FromContext(ctx); ok && !target.MerchantID.IsZero() {
		return target.MerchantID, true, nil
	}
	if mid, ok := merchant.HostMerchant(ctx); ok {
		return mid, true, nil
	}
	if mid, ok := merchant.FromContext(ctx); ok && !mid.IsZero() {
		return mid, true, nil
	}
	selector, present, err := merchant.ParseSelector(r.Header)
	if !present {
		return billing.MerchantID{}, false, nil
	}
	if err != nil {
		return billing.MerchantID{}, false, credential.ErrResourceTokenInvalid
	}
	if !selector.ID.IsZero() {
		return selector.ID, true, nil
	}
	mid, _, err := c.MerchantScope(ctx, selector.Slug)
	if err != nil {
		return billing.MerchantID{}, false, credential.ErrResourceTokenMerchantNotBound
	}
	return mid, true, nil
}

// unverifiedIssuer reads the token's iss only to pick its verifier; the
// verifier then checks it against the signature.
func unverifiedIssuer(r *http.Request) string {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 {
		return ""
	}
	parts := strings.Split(fields[1], ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return strings.TrimSpace(claims.Iss)
}

// resourceTokenError maps a verification failure to the gate's errors,
// carrying the DPoP challenge (WWW-Authenticate, DPoP-Nonce) the client
// retries with.
func resourceTokenError(r *http.Request, err error) error {
	challenge := headerRecorder{}
	verify.DPoPChallenge(challenge, r, err)
	headers := map[string]string{}
	for name, values := range challenge {
		if len(values) > 0 {
			headers[name] = values[0]
		}
	}
	switch {
	case errors.Is(err, verify.ErrSenderProofUnavailable):
		return credential.ErrResourceTokenUnavailable
	case headers["Dpop-Nonce"] != "":
		return credential.ChallengeError{Code: billing.CodeDPoPNonceRequired, Headers: headers, Err: err}
	case strings.Contains(headers["Www-Authenticate"], "invalid_dpop_proof"):
		return credential.ChallengeError{Code: billing.CodeSenderProofRequired, Headers: headers, Err: err}
	case errors.Is(err, iam.ErrTokenExpired):
		return credential.ChallengeError{Code: billing.CodeCredentialExpired, Headers: headers, Err: err}
	}
	return credential.ChallengeError{Code: billing.CodeAccessTokenInvalid, Headers: headers, Err: credential.ErrResourceTokenInvalid}
}

// headerRecorder is the ResponseWriter verify.DPoPChallenge sets its headers on.
type headerRecorder http.Header

func (h headerRecorder) Header() http.Header       { return http.Header(h) }
func (headerRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (headerRecorder) WriteHeader(int)             {}
