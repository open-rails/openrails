package billingauth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// DelegatedPrincipal is the resolved identity a host supplies for the
// browser-direct SELF-SERVICE surface (/v1/self/* and /v1/admin/*,
// issue #339). It is the framework-neutral counterpart of the control plane's
// resolved delegated token: the host verifies the incoming credential however
// it likes (its own federated-issuer registry, a session, a gateway header)
// and returns the EXPLICITLY mapped {tenant, subject, permissions} principal.
//
// EXPLICIT SUBSYSTEM MAPPING — NO FALLBACKS: the host must resolve BOTH the
// OpenRails merchant and the acting subject itself. OpenRails performs no
// try-merchant-then-subject (or any other implicit) resolution on a
// host-supplied principal; a principal with an empty/invalid tenant or
// subject is rejected with 401 (fail closed).
type DelegatedPrincipal struct {
	// MerchantID is the resolved OpenRails merchant id in UUID string form
	// (REQUIRED). The mapping from the host's credential to this merchant is
	// per-deployment configuration owned by the host — explicit, never inferred.
	// There is no default merchant to fall back to (#336): a deployment must
	// resolve a real merchant id here.
	MerchantID string

	// MerchantSlug is the merchant's display/audit slug (optional).
	MerchantSlug string

	// SubjectID is the acting customer: the canonical end-user /
	// subject id every self-service read and write is scoped to (REQUIRED).
	// The host is responsible for presenting the CANONICAL id (the shared
	// subject across all of its issuers) — OpenRails uses this value verbatim
	// as the billing account key, exactly like a delegated token's
	// `delegated_sub`.
	SubjectID string

	// Issuer identifies the verifying issuer / host system for audit
	// (optional; e.g. the host's issuer URL or service name). It fills the
	// same audit slot as a delegated token's validated `iss`.
	Issuer string

	// Permissions are the granted permission strings. They must come from the
	// delegated catalog (`openrails:self:*` / `openrails:merchant:*`); OpenRails
	// rejects a principal carrying any service/operator grant, mirroring the
	// verify-time catalog gate on real delegated tokens. At least one
	// permission is required.
	Permissions []string

	// Email / EmailVerified / Username are optional non-authoritative contact
	// metadata (hosted checkout prefill etc.); authorization remains
	// SubjectID + Permissions.
	Email         string
	EmailVerified bool
	Username      string
}

// ErrDelegatedPrincipalInvalid indicates a host-supplied principal is missing
// its explicit tenant or subject mapping. OpenRails maps it to 401.
var ErrDelegatedPrincipalInvalid = errors.New("delegated principal requires an explicit tenant and subject")

// Validate enforces the explicit-mapping contract: a usable principal carries
// a non-empty merchant id and subject. (Merchant-id FORMAT and the permission
// catalog are enforced by the adapting middleware, which owns those types.)
func (p *DelegatedPrincipal) Validate() error {
	if p == nil || strings.TrimSpace(p.MerchantID) == "" || strings.TrimSpace(p.SubjectID) == "" {
		return ErrDelegatedPrincipalInvalid
	}
	return nil
}

// DelegatedAuthenticator is the host-pluggable auth boundary for the
// delegated self-service surface (issue #339). The control plane's
// delegated-token verifier is the default implementation; a host that
// verifies its own credentials (one system, one credential) implements this
// single method and passes it via the embed/embedded Options. It is pure
// net/http: implementers never touch gin, context keys, or status codes.
//
// Return ErrUnauthenticated (or any non-nil error) when the request carries
// no valid credential; OpenRails maps that to 401. A returned error's message
// is surfaced to the client, so it should be safe to expose.
type DelegatedAuthenticator interface {
	AuthenticateDelegated(ctx context.Context, r *http.Request) (*DelegatedPrincipal, error)
}

// DelegatedAuthenticatorFunc adapts an ordinary function to the
// DelegatedAuthenticator interface, so a host can pass a closure without
// declaring a type:
//
//	opts.DelegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(
//		func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
//			user, err := hostAuth.Verify(r) // the host's own credential check
//			if err != nil {
//				return nil, billingauth.ErrUnauthenticated
//			}
//			return &billingauth.DelegatedPrincipal{
//				MerchantID:    deploymentTenantID, // explicit per-deployment mapping
//				SubjectID:   user.CanonicalID,
//				Issuer:      "https://auth.host.example",
//				Permissions: []string{"openrails:self:billing:read"},
//			}, nil
//		})
type DelegatedAuthenticatorFunc func(ctx context.Context, r *http.Request) (*DelegatedPrincipal, error)

// AuthenticateDelegated implements DelegatedAuthenticator.
func (f DelegatedAuthenticatorFunc) AuthenticateDelegated(ctx context.Context, r *http.Request) (*DelegatedPrincipal, error) {
	return f(ctx, r)
}
