package engine

import (
	"context"
	"errors"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
)

// integration adapts the host's hooks to the engine's authentication and
// authorization boundary; nil when the host authenticates nobody.
func integration(deps config.Deps) *billingauth.Integration {
	if deps.Authenticate == nil {
		return nil
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
				return billingauth.GateError{Status: http.StatusUnauthorized, Message: billingauth.UnauthenticatedMessage(err)}
			case errors.Is(err, billingauth.ErrForbidden):
				return billingauth.GateError{Status: http.StatusForbidden, Message: "permission_required"}
			default:
				return err
			}
		})
	}
	if recent := deps.RecentSignIn; recent != nil {
		out.RecentSignIn = billingauth.RecentSignInFunc(func(_ context.Context, r *http.Request) error { return recent(r) })
	}
	return out
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

func userDirectory(deps config.Deps) billing.UserDirectory {
	if deps.UserExists == nil || deps.UserEmail == nil {
		return nil
	}
	return userDirectoryFuncs{exists: deps.UserExists, email: deps.UserEmail}
}

type usernameResolverFunc func(context.Context, string) (string, error)

func (f usernameResolverFunc) GetUserIDByUsername(ctx context.Context, username string) (string, error) {
	return f(ctx, username)
}

func usernameResolver(deps config.Deps) billing.UsernameResolver {
	if deps.ResolveUsername == nil {
		return nil
	}
	return usernameResolverFunc(deps.ResolveUsername)
}
