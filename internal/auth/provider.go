package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails/pkg/billingauth"
	log "github.com/sirupsen/logrus"
)

// Admission is the host's per-request veto, evaluated AFTER the credential
// verifies and BEFORE any principal is built (#918). It exists because JWT
// verification is stateless: a banned, deleted or otherwise inadmissible
// subject keeps a syntactically valid token until it expires, so a privileged
// surface has to re-ask a live authority. Any non-nil error rejects the
// request with billingauth.ErrUnauthenticated — the cause is logged, never
// returned to the client.
type Admission func(ctx context.Context, r *http.Request, cl verify.Claims) error

// Authenticator is the framework-neutral auth boundary backed by an AuthKit JWT
// verifier. It implements billingauth.Authenticator (the gin adapter package
// pkg/authprovider/ginauth was removed with the gin exit, #670).
type Authenticator struct {
	cfg AuthenticatorConfig
}

// AuthenticatorConfig is Authenticator's construction input.
type AuthenticatorConfig struct {
	// Verifier verifies the request's credential (required).
	Verifier RequestVerifier
	// Admit is the optional per-request admission veto (#918).
	Admit Admission
	// OmitTokenRoles drops the token's role snapshot from the resulting
	// UserContext. A JWT role list is stale for the token's lifetime, so a host
	// that authorizes from a live authority instead (doujins #774) sets this
	// rather than passing a snapshot nothing should read.
	OmitTokenRoles bool
}

// NewAuthenticator builds the gin-free AuthKit-backed Authenticator.
// Standalone passes the control-plane verifier; embedded hosts pass their own.
func NewAuthenticator(cfg AuthenticatorConfig) *Authenticator {
	return &Authenticator{cfg: cfg}
}

// Authenticate implements billingauth.Authenticator so the AuthKit-backed
// provider can drive the gin-free embedded surface (issue #282). It verifies the
// bearer token and returns the resulting UserContext, returning
// billingauth.ErrUnauthenticated when no/invalid credential is present.
func (p *Authenticator) Authenticate(ctx context.Context, r *http.Request) (billingauth.UserContext, error) {
	if p == nil || p.cfg.Verifier == nil || r == nil {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	cl, err := p.cfg.Verifier.VerifyRequest(r)
	if err != nil {
		return billingauth.UserContext{}, err
	}
	if err := admit(ctx, r, cl, p.cfg.Admit); err != nil {
		return billingauth.UserContext{}, err
	}
	return userContextFromClaims(cl, p.cfg.OmitTokenRoles), nil
}

// admit runs the host's admission veto fail-closed: the host's own error is
// logged against the subject and the caller sees only ErrUnauthenticated,
// since a DelegatedAuthenticator error message reaches the client.
func admit(ctx context.Context, r *http.Request, cl verify.Claims, a Admission) error {
	if a == nil {
		return nil
	}
	if err := a(ctx, r, cl); err != nil {
		log.WithError(err).WithField("subject", cl.UserID).
			Info("host admission refused a verified credential")
		return billingauth.ErrUnauthenticated
	}
	return nil
}

// looksLikeJWT mirrors controlplane.LooksLikeJWT (three dot-separated
// segments) without importing the control plane.
func looksLikeJWT(token string) bool {
	return strings.Count(strings.TrimSpace(token), ".") == 2
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(strings.ToLower(header), strings.ToLower(prefix)) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func userContextFromClaims(cl verify.Claims, omitRoles bool) billingauth.UserContext {
	uc := billingauth.UserContext{
		UserID:          cl.UserID,
		Email:           cl.Email,
		EmailVerified:   cl.EmailVerified,
		Username:        cl.Username,
		DiscordUsername: cl.DiscordUsername,
		SessionID:       cl.SessionID,
		Roles:           cl.Roles,
		Entitlements:    cl.Entitlements,
		// #567: the permission-group model has no legacy tenant persona, so a user
		// access token carries no group slug/group roles. Merchant-admin authority
		// is a role on the merchant permission-group, evaluated live by
		// HasAdminPermission against the merchant the route resolves — not read from the token. Merchant is
		// left empty here; it is populated only on the in-process/delegated paths
		// that bind an explicit merchant (see internal/http/routes + middleware).
	}
	if omitRoles {
		uc.Roles = nil
	}
	return uc
}
