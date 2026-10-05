package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/identity"
	"net/http"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
)

// integration is the engine's authentication and authorization boundary:
// derived from the host's AuthKit, or adapted from its own hooks. Nil when
// the host authenticates nobody.
func integration(deps config.Deps) (*billingauth.Integration, error) {
	if deps.AuthKit != nil {
		options := billingauth.IntegrationOptions{Verifier: deps.AuthKit, Customer: billingauth.SubjectCustomerID}
		if customerFor := deps.CustomerFor; customerFor != nil {
			options.Customer = func(ctx context.Context, principal auth.Principal) (billingauth.CustomerIdentity, error) {
				caller, ok := billingauth.IdentityOf(principal.Identity())
				if !ok {
					return billingauth.CustomerIdentity{}, billingauth.ErrUnauthenticated
				}
				id, err := customerFor(ctx, caller)
				return billingauth.CustomerIdentity{ID: id}, err
			}
		}
		if deps.AuthorityFor != nil {
			options.Authority = billingauth.AuthorityResolver(deps.AuthorityFor)
		}
		out, err := billingauth.NewIntegration(options)
		if err != nil {
			return nil, fmt.Errorf("openrails: Deps.AuthKit: %w", err)
		}
		return out, nil
	}
	if deps.Authenticate == nil {
		return nil, nil
	}
	authenticate := deps.Authenticate
	out := &billingauth.Integration{Authentication: billingauth.AuthenticationFunc(func(_ context.Context, r *http.Request) (billingauth.Identity, error) {
		identity, err := authenticate(r)
		if err != nil {
			return billingauth.Identity{}, err
		}
		// A native user's own credential is a user session; anything else
		// is automation unless the host says otherwise.
		if identity.CredentialClass == billingauth.CredentialClassUnknown {
			identity.CredentialClass = billingauth.CredentialClassAutomation
			if identity.Kind == billingauth.User {
				identity.CredentialClass = billingauth.CredentialClassUserSession
			}
		}
		return identity, nil
	})}
	if authorize := deps.Authorize; authorize != nil {
		out.Authorization = billingauth.AuthorizationFunc(func(_ context.Context, r *http.Request, identity billingauth.Identity, required billingauth.Requirement) error {
			err := authorize(r, identity, required)
			var gate billingauth.GateError
			switch {
			case err == nil, errors.As(err, &gate):
				return err
			case errors.Is(err, billingauth.ErrUnauthenticated):
				return billingauth.Unauthenticated(err)
			case errors.Is(err, billingauth.ErrForbidden):
				return billingauth.Refusal(billing.CodePermissionRequired)
			default:
				return err
			}
		})
	}
	if recent := deps.RecentSignIn; recent != nil {
		out.RecentSignIn = billingauth.RecentSignInFunc(func(_ context.Context, r *http.Request) error { return recent(r) })
	}
	return out, nil
}

// checkoutCustomer is the checkout session account check: the host's hook, else
// the AuthKit user when customers are AuthKit users.
func checkoutCustomer(deps config.Deps) func(context.Context, billing.CustomerID) (billing.CheckoutCustomerIdentity, error) {
	if deps.CheckoutCustomer != nil {
		return deps.CheckoutCustomer
	}
	directory, ok := deps.AuthKit.(interface {
		Users(context.Context, []string) (map[string]iam.User, error)
	})
	if !ok || deps.CustomerFor != nil {
		return nil
	}
	return func(ctx context.Context, customerID billing.CustomerID) (billing.CheckoutCustomerIdentity, error) {
		users, err := directory.Users(ctx, []string{customerID.String()})
		if err != nil {
			return billing.CheckoutCustomerIdentity{}, err
		}
		user, ok := users[customerID.String()]
		if !ok || user.Ban != nil || user.DeletedAt != nil {
			return billing.CheckoutCustomerIdentity{}, billingauth.ErrForbidden
		}
		out := billing.CheckoutCustomerIdentity{ID: customerID, Username: user.Username}
		if user.EmailVerified && user.Email != nil {
			out.VerifiedEmail = *user.Email
		}
		return out, nil
	}
}

type userDirectoryFuncs struct {
	exists func(context.Context, string) (bool, error)
	email  func(context.Context, string) (string, string, bool, error)
}

func (d userDirectoryFuncs) Exists(ctx context.Context, userID string) (bool, error) {
	return d.exists(ctx, userID)
}

func (d userDirectoryFuncs) EmailIdentity(ctx context.Context, userID string) (string, string, bool, error) {
	return d.email(ctx, userID)
}

func userDirectory(deps config.Deps) identity.UserDirectory {
	if deps.UserExists == nil || deps.UserEmail == nil {
		return nil
	}
	return userDirectoryFuncs{exists: deps.UserExists, email: deps.UserEmail}
}

type usernameResolverFunc func(context.Context, string) (string, error)

func (f usernameResolverFunc) GetUserIDByUsername(ctx context.Context, username string) (string, error) {
	return f(ctx, username)
}

func usernameResolver(deps config.Deps) identity.UsernameResolver {
	if deps.ResolveUsername == nil {
		return nil
	}
	return usernameResolverFunc(deps.ResolveUsername)
}
