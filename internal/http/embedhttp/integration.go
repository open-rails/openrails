package embedhttp

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/pkg/billingauth"
)

type integrationAuthenticator struct{ auth *billingauth.Integration }

func (a integrationAuthenticator) Authenticate(ctx context.Context, r *http.Request) (billingauth.UserContext, error) {
	if a.auth == nil || a.auth.Authentication == nil {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	identity, err := a.auth.Authentication.AuthenticateRequest(ctx, r)
	if err != nil {
		return billingauth.UserContext{}, err
	}
	if identity.Kind != billingauth.NativeUser || identity.CredentialClass != billingauth.CredentialClassUserSession || identity.Invoker != "" {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	return billingauth.UserContext{UserID: identity.SubjectID, Email: identity.Email, EmailVerified: identity.EmailVerified, Username: identity.Username, SessionID: identity.SessionID}, nil
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
		if err := merchanttarget.Assert(r, billingauth.Target{MerchantID: host.MerchantID, MerchantSlug: host.MerchantSlug}); err != nil {
			return billingauth.Principal{}, err
		}
		return billingauth.Principal{MerchantID: host.MerchantID, Subject: host.Subject, Permissions: append([]string(nil), host.Permissions...)}, nil
	}
	if g.auth == nil || g.auth.Authentication == nil || g.auth.Authorization == nil {
		return billingauth.Principal{}, billingauth.GateError{Status: 503, Message: "authorization unavailable"}
	}
	identity, err := g.auth.Authentication.AuthenticateRequest(ctx, r)
	if err != nil {
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
	required := billingauth.Requirement{Permission: permission, Scope: scope, Target: target}
	if err := g.auth.Authorization.Authorize(ctx, r, identity, required); err != nil {
		return billingauth.Principal{}, err
	}
	principal := billingauth.Principal{MerchantID: target.MerchantID, Subject: identity.SubjectID}
	if identity.Kind == billingauth.NativeUser {
		principal.UserContext = billingauth.UserContext{UserID: identity.SubjectID, Email: identity.Email, EmailVerified: identity.EmailVerified, Username: identity.Username, Merchant: target.MerchantSlug}
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
		identity, err := auth.Authentication.AuthenticateRequest(ctx, r)
		if err != nil {
			return nil, err
		}
		if identity.Kind != billingauth.NativeUser || identity.SubjectID == "" || identity.CredentialClass != billingauth.CredentialClassUserSession || identity.Invoker != "" {
			return nil, billingauth.ErrUnauthenticated
		}
		if err := merchanttarget.Assert(r, target); err != nil {
			return nil, err
		}
		return &billingauth.DelegatedPrincipal{MerchantID: target.MerchantID.String(), MerchantSlug: target.MerchantSlug, SubjectID: identity.SubjectID, Issuer: identity.Issuer, CredentialClass: identity.CredentialClass, Email: identity.Email, EmailVerified: identity.EmailVerified, Username: identity.Username}, nil
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
