package embedhttp

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

type integrationAuthenticator struct{ auth *billingauth.Integration }

func (a integrationAuthenticator) Authenticate(ctx context.Context, r *http.Request) (billingauth.UserContext, error) {
	if a.auth == nil || a.auth.Authentication == nil {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	identity, err := authenticateIntegration(ctx, r, a.auth)
	if err != nil {
		return billingauth.UserContext{}, err
	}
	if identity.Kind != billingauth.NativeUser || identity.CustomerID == "" || identity.CredentialClass != billingauth.CredentialClassUserSession || identity.Invoker != "" {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	return billingauth.UserContext{UserID: identity.CustomerID, Email: identity.Email, EmailVerified: identity.EmailVerified, Username: identity.Username, SessionID: identity.SessionID}, nil
}

type integrationGate struct {
	auth    *billingauth.Integration
	runtime *app.Runtime
}

func (g integrationGate) Authorize(ctx context.Context, r *http.Request, permission string) (billingauth.Principal, error) {

	if host, ok := requestauth.HostPrincipalFromContext(ctx); ok {
		if host.MerchantID.IsZero() || !billingauth.HasPermission(host.Permissions, permission) {
			return billingauth.Principal{}, billingauth.GateError{Status: 403, Message: "permission_required"}
		}
		target := billingauth.Target{MerchantID: host.MerchantID, MerchantSlug: host.MerchantSlug}
		if captured, ok := merchanttarget.FromContext(ctx); ok {
			if captured.MerchantID != host.MerchantID {
				return billingauth.Principal{}, billingauth.GateError{Status: 409, Message: "host merchant binding mismatch"}
			}
			target = captured
		}
		if err := merchanttarget.Assert(r, target); err != nil {
			return billingauth.Principal{}, err
		}
		return billingauth.Principal{MerchantID: host.MerchantID, Subject: host.Subject, Permissions: append([]string(nil), host.Permissions...)}, nil
	}
	if g.auth == nil || g.auth.Authentication == nil || g.auth.Authorization == nil {
		return billingauth.Principal{}, billingauth.GateError{Status: 503, Message: "authorization unavailable"}
	}
	identity, err := authenticateIntegration(ctx, r, g.auth)
	if err != nil {
		var gate billingauth.GateError
		if errors.As(err, &gate) && gate.Status == http.StatusServiceUnavailable {
			return billingauth.Principal{}, err
		}
		return billingauth.Principal{}, billingauth.GateError{Status: 401, Message: billingauth.UnauthenticatedMessage(err)}
	}
	target, err := merchanttarget.Resolve(ctx, r, g.runtime.Merchants, g.runtime.ConfiguredMerchant(), "")
	if err != nil {
		return billingauth.Principal{}, err
	}
	*r = *r.WithContext(merchanttarget.WithResolved(r.Context(), target))
	scope := billingauth.MerchantScope
	if strings.HasPrefix(permission, "root:") {
		scope = billingauth.PlatformScope
	}
	if identity.Kind != billingauth.NativeUser && !billingauth.HasPermission(identity.Permissions, permission) {
		return billingauth.Principal{}, billingauth.GateError{Status: 403, Message: "credential permission ceiling"}
	}
	subject := identity.SubjectID
	if identity.Kind == billingauth.NativeUser && (permission == permissions.MerchantCatalogOwnRead || permission == permissions.MerchantCatalogOwnUpdate) {
		if identity.CustomerID == "" {
			return billingauth.Principal{}, billingauth.GateError{Status: 403, Message: "canonical personal identity required"}
		}
		subject = identity.CustomerID
	}
	required := billingauth.Requirement{Permission: permission, Scope: scope, Target: target}
	if err := g.auth.Authorization.Authorize(ctx, r, identity, required); err != nil {
		var gate billingauth.GateError
		if errors.As(err, &gate) {
			return billingauth.Principal{}, err
		}
		return billingauth.Principal{}, billingauth.GateError{Status: http.StatusServiceUnavailable, Message: "authorization unavailable"}
	}
	principal := billingauth.Principal{MerchantID: target.MerchantID, Subject: subject}
	if identity.Kind == billingauth.NativeUser {
		principal.UserContext = billingauth.UserContext{UserID: identity.CustomerID, Email: identity.Email, EmailVerified: identity.EmailVerified, Username: identity.Username, Merchant: target.MerchantSlug}
	} else {
		principal.Permissions = append([]string(nil), identity.Permissions...)
	}
	return principal, nil
}

func nativeCustomer(auth *billingauth.Integration, target billingauth.Target) billingauth.DelegatedAuthenticator {
	return billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		if auth == nil || auth.Authentication == nil {
			return nil, billingauth.ErrUnauthenticated
		}
		identity, err := authenticateIntegration(ctx, r, auth)
		if err != nil {
			return nil, err
		}
		if identity.Kind != billingauth.NativeUser || identity.CustomerID == "" || identity.CredentialClass != billingauth.CredentialClassUserSession || identity.Invoker != "" {
			return nil, billingauth.ErrUnauthenticated
		}
		requestTarget := target
		if resolved, ok := merchanttarget.FromContext(r.Context()); ok {
			// The v2 protocol resolved this request before authentication. A
			// renamed slug may still name this fixed book; a reused slug may not.
			if resolved.MerchantID != target.MerchantID {
				return nil, billingauth.GateError{Status: 409, Message: "customer merchant binding mismatch"}
			}
			requestTarget = resolved
		} else if strings.TrimSpace(r.Header.Get(merchant.SlugHeader)) != "" {
			return nil, billingauth.GateError{Status: 400, Message: "merchant slug selection requires v2"}
		}
		if err := merchanttarget.Assert(r, requestTarget); err != nil {
			return nil, err
		}
		*r = *r.WithContext(merchanttarget.WithResolved(r.Context(), requestTarget))
		return &billingauth.DelegatedPrincipal{MerchantID: requestTarget.MerchantID.String(), MerchantSlug: requestTarget.MerchantSlug, SubjectID: identity.CustomerID, Issuer: identity.Issuer, CredentialClass: identity.CredentialClass, Email: identity.Email, EmailVerified: identity.EmailVerified, Username: identity.Username}, nil
	})
}

// IntegrationGate is shared by published routes and the headless Client handler.
func IntegrationGate(rt *app.Runtime) billingauth.Gate {
	return integrationGate{auth: rt.Auth, runtime: rt}
}

// RuntimeCustomerAuthentication resolves the Client's explicit target before
// binding its native identity, without requiring HTTP publication.
func RuntimeCustomerAuthentication(rt *app.Runtime) billingauth.DelegatedAuthenticator {
	return billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		target, err := merchanttarget.Resolve(ctx, r, rt.Merchants, rt.ConfiguredMerchant(), "")
		if err != nil {
			return nil, err
		}
		return nativeCustomer(rt.Auth, target).AuthenticateDelegated(ctx, r)
	})
}

func authenticateIntegration(ctx context.Context, r *http.Request, auth *billingauth.Integration) (billingauth.Identity, error) {
	return requestauth.Once(ctx, auth, func() (billingauth.Identity, error) {
		identity, err := auth.Authentication.AuthenticateRequest(ctx, r)
		if err != nil {
			return billingauth.Identity{}, err
		}
		if (identity.Kind != billingauth.Machine && strings.TrimSpace(identity.SubjectID) == "") || strings.TrimSpace(identity.Issuer) == "" {
			return billingauth.Identity{}, billingauth.ErrUnauthenticated
		}
		switch identity.Kind {
		case billingauth.NativeUser:
			id, err := uuid.Parse(identity.CustomerID)
			if (identity.CustomerID != "" && (err != nil || id == uuid.Nil || id.String() != identity.CustomerID)) || identity.CredentialClass != billingauth.CredentialClassUserSession || identity.Invoker != "" {
				return billingauth.Identity{}, billingauth.ErrUnauthenticated
			}
		case billingauth.Machine, billingauth.DelegatedUser:
			if identity.CredentialClass == billingauth.CredentialClassUserSession {
				return billingauth.Identity{}, billingauth.ErrUnauthenticated
			}
		default:
			return billingauth.Identity{}, billingauth.ErrUnauthenticated
		}
		return identity, nil
	})
}
