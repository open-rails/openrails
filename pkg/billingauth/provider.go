package billingauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"
	auth "github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/internal/requestauth"
)

// Verifier authenticates a complete request, including any required sender proof.
// Providers satisfy this interface without importing OpenRails.
type Verifier interface {
	AuthenticateRequest(context.Context, *http.Request) (auth.Principal, error)
}

// CustomerIdentity is the host's explicit mapping to a billing account. A
// delegated or machine credential must never become a customer-present session.
type CustomerIdentity struct {
	ID              string
	CredentialClass CredentialClass
	Invoker         string
}

type CustomerResolver func(context.Context, auth.Principal) (CustomerIdentity, error)

// SubjectCustomerID explicitly selects the host policy that native user UUIDs
// are also payable customer UUIDs. Other credential kinds receive no mapping.
func SubjectCustomerID(_ context.Context, principal auth.Principal) (CustomerIdentity, error) {
	i := principal.Identity()
	if i.Kind != auth.KindUser {
		return CustomerIdentity{}, nil
	}
	id, err := uuid.Parse(i.Subject)
	if err != nil || id == uuid.Nil || id.String() != i.Subject {
		return CustomerIdentity{}, ErrUnauthenticated
	}
	return CustomerIdentity{ID: i.Subject, CredentialClass: CredentialClassUserSession}, nil
}

// Authority maps a billing requirement to one trusted immutable authority scope.
type Authority struct {
	Scope      auth.Scope
	Permission string
}

type AuthorityResolver func(context.Context, Requirement) (Authority, error)

type IntegrationOptions struct {
	Verifier Verifier
	Customer CustomerResolver
	// Authority is optional for personal customer routes. Privileged operations
	// require this mapping and the verified principal's PermissionChecker.
	Authority AuthorityResolver
	// PlatformAuthority is an explicit alternative authority for the exact
	// operation. It never derives from token roles or directory-read access.
	PlatformAuthority AuthorityResolver
}

type providerIntegration struct{ options IntegrationOptions }

// NewIntegration adapts a host verifier to billing identity and live authority.
// Verification is cached for the current request; permission decisions are not.
func NewIntegration(options IntegrationOptions) (*Integration, error) {
	if nilInterface(options.Verifier) {
		return nil, fmt.Errorf("billing authentication: verifier is required")
	}
	p := &providerIntegration{options: options}
	result := &Integration{Authentication: p}
	if options.Authority != nil || options.PlatformAuthority != nil {
		result.Authorization = p
	}
	return result, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (p *providerIntegration) principal(ctx context.Context, r *http.Request) (auth.Principal, error) {
	if r == nil {
		return nil, ErrUnauthenticated
	}
	return requestauth.Once(ctx, p, func() (auth.Principal, error) {
		principal, err := p.options.Verifier.AuthenticateRequest(ctx, r)
		if err != nil {
			return nil, authenticationFailure(err)
		}
		if nilInterface(principal) {
			return nil, ErrUnauthenticated
		}
		return principal, nil
	})
}

func (p *providerIntegration) AuthenticateRequest(ctx context.Context, r *http.Request) (Identity, error) {
	return requestauth.Once(ctx, &p.options, func() (Identity, error) { return p.identity(ctx, r) })
}

func (p *providerIntegration) identity(ctx context.Context, r *http.Request) (Identity, error) {
	principal, err := p.principal(ctx, r)
	if err != nil {
		return Identity{}, err
	}
	i := principal.Identity()
	out := Identity{Issuer: i.Issuer, SubjectID: i.Subject, Email: i.Email, EmailVerified: i.EmailVerified, Username: i.Username, SessionID: i.SessionID, CredentialClass: CredentialClassAutomation}
	if strings.TrimSpace(i.Issuer) == "" || strings.TrimSpace(i.Subject) == "" {
		return Identity{}, ErrUnauthenticated
	}
	switch i.Kind {
	case auth.KindUser:
		out.Kind, out.CredentialClass = NativeUser, CredentialClassUserSession
	case auth.KindDelegated:
		out.Kind = DelegatedUser
	case auth.KindAPIKey, auth.KindRemoteApplication, auth.KindDeviceKey:
		out.Kind = Machine
	default:
		return Identity{}, ErrUnauthenticated
	}
	if p.options.Customer != nil {
		customer, err := p.options.Customer(ctx, principal)
		if err != nil {
			return Identity{}, authenticationFailure(err)
		}
		out.CustomerID, out.Invoker = customer.ID, customer.Invoker
		if customer.CredentialClass != CredentialClassUnknown {
			out.CredentialClass = customer.CredentialClass
		}
		if i.Kind != auth.KindUser && out.CredentialClass == CredentialClassUserSession {
			return Identity{}, ErrUnauthenticated
		}
	}
	return out, nil
}

func (p *providerIntegration) Authorize(ctx context.Context, r *http.Request, identity Identity, required Requirement) error {
	principal, err := p.principal(ctx, r)
	if err != nil {
		return err
	}
	verified, err := p.AuthenticateRequest(ctx, r)
	if err != nil {
		return err
	}
	if identity.Kind != verified.Kind || identity.Issuer != verified.Issuer || identity.SubjectID != verified.SubjectID || identity.CustomerID != verified.CustomerID || identity.CredentialClass != verified.CredentialClass || identity.Invoker != verified.Invoker || !slices.Equal(identity.Permissions, verified.Permissions) {
		return GateError{Status: http.StatusUnauthorized, Message: "credential identity mismatch"}
	}
	checker, ok := principal.(auth.PermissionChecker)
	if !ok || nilInterface(checker) {
		return GateError{Status: http.StatusForbidden, Message: "permission_required"}
	}
	for index, resolve := range []AuthorityResolver{p.options.Authority, p.options.PlatformAuthority} {
		if resolve == nil {
			continue
		}
		query := required
		if index == 1 {
			query.Scope = PlatformScope
		}
		mapping, err := resolve(ctx, query)
		if err != nil {
			return GateError{Status: http.StatusServiceUnavailable, Message: "authorization unavailable"}
		}
		if mapping.Permission == "" || mapping.Scope.ID == "" || mapping.Scope.Authority == "" {
			continue
		}
		if index == 0 && required.Target.AuthorityGroupID != "" && mapping.Scope.ID != required.Target.AuthorityGroupID {
			continue
		}
		allowed, err := checker.Can(ctx, mapping.Scope, mapping.Permission)
		if err != nil {
			return GateError{Status: http.StatusServiceUnavailable, Message: "authorization unavailable"}
		}
		if allowed {
			return nil
		}
	}
	return GateError{Status: http.StatusForbidden, Message: "permission_required"}
}

func authenticationFailure(err error) error {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		return GateError{Status: http.StatusForbidden, Message: "permission_required"}
	case errors.Is(err, auth.ErrUnavailable):
		return GateError{Status: http.StatusServiceUnavailable, Message: "authentication unavailable"}
	case errors.Is(err, auth.ErrSenderProofRequired):
		return GateError{Status: http.StatusUnauthorized, Message: "sender_proof_required"}
	case errors.Is(err, auth.ErrExpired):
		return GateError{Status: http.StatusUnauthorized, Message: "credential_expired"}
	case errors.Is(err, auth.ErrRevoked):
		return GateError{Status: http.StatusUnauthorized, Message: "credential_revoked"}
	default:
		return ErrUnauthenticated
	}
}
