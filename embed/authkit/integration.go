package authkit

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	coreauth "github.com/open-rails/authkit"
	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// Authority is an explicit permission-to-group mapping. Group is resolved once
// through Client and its immutable ID is used for the live permission check.
type Authority struct {
	Group      coreauth.GroupRef
	Permission coreauth.Perm
}

type AuthorityResolver func(context.Context, billingauth.Requirement) (Authority, error)

// Config constructs the optional AuthKit integration. A verifier alone supports
// native personal customer routes. Privileged routes need Client and an explicit
// authority resolver; token role names never provide a fallback.
type Config struct {
	Verifier  Verifier
	Client    coreauth.Client
	Authority AuthorityResolver
	// PlatformAuthority is an explicit operation-specific root override. It is
	// never inferred from a role name or a generic directory-read permission.
	PlatformAuthority AuthorityResolver
	// AuthorityIssuer fences machine group ownership. It is the receiving AuthKit
	// deployment's issuer, not an issuer copied from the incoming credential.
	AuthorityIssuer string
	// Admission is explicitly opt-in; native bans are otherwise applied on token
	// refresh/expiry by AuthKit, while permission checks below remain live.
	Admission Admission
}

type integration struct{ cfg Config }

func New(cfg Config) (*billingauth.Integration, error) {
	if cfg.Verifier == nil || (reflect.ValueOf(cfg.Verifier).Kind() == reflect.Pointer && reflect.ValueOf(cfg.Verifier).IsNil()) {
		return nil, fmt.Errorf("authkit integration: verifier is required")
	}
	if (cfg.Client == nil) != (cfg.Authority == nil && cfg.PlatformAuthority == nil) {
		return nil, fmt.Errorf("authkit integration: privileged authority requires both Client and Authority")
	}
	p := &integration{cfg: cfg}
	result := &billingauth.Integration{Authentication: p}
	if cfg.Client != nil {
		result.Authorization = p
	}
	return result, nil
}

func (p *integration) claims(ctx context.Context, r *http.Request) (verify.Claims, error) {
	if r == nil {
		return verify.Claims{}, billingauth.ErrUnauthenticated
	}
	return requestauth.Once(ctx, p, func() (verify.Claims, error) {
		cl, err := p.cfg.Verifier.VerifyRequest(r)
		if err != nil {
			return verify.Claims{}, err
		}
		if p.cfg.Admission != nil {
			if err := p.cfg.Admission(ctx, r, cl); err != nil {
				return verify.Claims{}, billingauth.ErrUnauthenticated
			}
		}
		return cl, nil
	})
}

func identityFromClaims(cl verify.Claims) (billingauth.Identity, error) {
	out := billingauth.Identity{Issuer: cl.Issuer, Email: cl.Email, EmailVerified: cl.EmailVerified, Username: cl.Username, SessionID: cl.SessionID}
	switch cl.PrincipalKind() {
	case coreauth.PrincipalKindUser:
		if !cl.IsUser() || cl.DeviceKeyID != "" || cl.TokenType != "" {
			return out, billingauth.ErrUnauthenticated
		}
		out.Kind = billingauth.NativeUser
		out.SubjectID = cl.UserID
		out.CustomerID = cl.UserID
		out.CredentialClass = billingauth.CredentialClassUserSession
	case coreauth.PrincipalKindAPIKey:
		out.Kind = billingauth.Machine
		out.Issuer = cl.PermissionGroupAuthorityIssuer
		out.CredentialClass = billingauth.CredentialClassAutomation
		out.Permissions = append([]string(nil), cl.Permissions...)
	case coreauth.PrincipalKindRemoteApplication:
		out.Kind = billingauth.Machine
		out.SubjectID = cl.RemoteApplicationID
		out.CredentialClass = billingauth.CredentialClassAutomation
		out.Permissions = append([]string(nil), cl.Permissions...)
	case coreauth.PrincipalKindDelegated:
		// This is a credential ceiling, never a native user to look up locally.
		if !cl.IsDelegatedAccessToken() {
			return out, billingauth.ErrUnauthenticated
		}
		out.Kind = billingauth.DelegatedUser
		out.SubjectID = cl.DelegatedSubject
		out.CredentialClass = billingauth.CredentialClassAutomation
		out.Permissions = append([]string(nil), cl.Permissions...)
	default:
		return out, billingauth.ErrUnauthenticated
	}
	return out, nil
}

func (p *integration) AuthenticateRequest(ctx context.Context, r *http.Request) (billingauth.Identity, error) {
	cl, err := p.claims(ctx, r)
	if err != nil {
		return billingauth.Identity{}, err
	}
	return identityFromClaims(cl)
}

func (p *integration) Authorize(ctx context.Context, r *http.Request, identity billingauth.Identity, required billingauth.Requirement) error {
	cl, err := p.claims(ctx, r)
	if err != nil {
		return billingauth.GateError{Status: 401, Message: billingauth.UnauthenticatedMessage(err)}
	}
	verified, err := identityFromClaims(cl)
	if err != nil || identity.Kind != verified.Kind || identity.SubjectID != verified.SubjectID || identity.Issuer != verified.Issuer || identity.CustomerID != verified.CustomerID || identity.CredentialClass != verified.CredentialClass || identity.Invoker != verified.Invoker || !slices.Equal(identity.Permissions, verified.Permissions) {
		return billingauth.GateError{Status: 401, Message: "credential identity mismatch"}
	}
	if p.cfg.Client == nil {
		return billingauth.GateError{Status: 503, Message: "authorization unavailable"}
	}
	if strings.TrimSpace(required.Permission) == "" {
		return billingauth.GateError{Status: 403, Message: "permission_required"}
	}
	allowed, err := p.checkAuthority(ctx, cl, required, p.cfg.Authority, false)
	if err != nil {
		return err
	}
	if !allowed && p.cfg.PlatformAuthority != nil {
		platform := required
		platform.Scope = billingauth.PlatformScope
		allowed, err = p.checkAuthority(ctx, cl, platform, p.cfg.PlatformAuthority, true)
		if err != nil {
			return err
		}
	}
	if !allowed {
		return billingauth.GateError{Status: 403, Message: "permission_required"}
	}
	return nil
}

func (p *integration) checkAuthority(ctx context.Context, cl verify.Claims, required billingauth.Requirement, resolve AuthorityResolver, platform bool) (bool, error) {
	if resolve == nil {
		return false, nil
	}
	mapping, err := resolve(ctx, required)
	if err != nil {
		return false, billingauth.GateError{Status: 503, Message: "authorization unavailable"}
	}
	if mapping.Permission == "" || mapping.Group.Persona == "" {
		return false, nil
	}
	if mapping.Permission.Persona() != mapping.Group.Persona || (platform && !mapping.Group.IsRoot()) {
		return false, nil
	}
	group, err := p.cfg.Client.GroupInstanceForSlug(ctx, mapping.Group)
	if err != nil {
		return false, billingauth.GateError{Status: 503, Message: "authorization unavailable"}
	}
	if required.Target.AuthorityGroupID != "" && !platform && group.ID != required.Target.AuthorityGroupID {
		return false, nil
	}
	scope := verify.PermissionScope{GroupID: group.ID, AuthorityIssuer: p.cfg.AuthorityIssuer, Persona: group.Persona, Instance: group.InstanceSlug}
	if cl.PrincipalKind() != coreauth.PrincipalKindUser && !cl.BoundToPermissionGroup() {
		return false, nil
	}
	allowed, err := verify.Allow(ctx, p.cfg.Client, cl, mapping.Permission, scope)
	if err != nil {
		return false, billingauth.GateError{Status: 503, Message: "authorization unavailable"}
	}
	return allowed, nil
}
