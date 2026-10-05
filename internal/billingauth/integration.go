package billingauth

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// PrincipalKind is verified credential provenance, not a role or permission.
type PrincipalKind string

const (
	User      PrincipalKind = "user"
	Machine   PrincipalKind = "machine"
	Delegated PrincipalKind = "delegated"
)

// Identity is authentication output. Native users carry no role/permission
// snapshot. Permissions belongs only to verified non-user credential ceilings.
type Identity struct {
	Kind      PrincipalKind
	SubjectID string
	// CustomerID is the customer the host maps (Issuer, SubjectID) to.
	// Required for customer, checkout and personal catalog operations,
	// optional for merchant staff. OpenRails never guesses this mapping.
	CustomerID      billing.CustomerID
	Issuer          string
	CredentialClass CredentialClass
	Invoker         string
	Permissions     []string
	Email           string
	EmailVerified   bool
	Username        string
	SessionID       string
}

type Scope string

const (
	MerchantScope Scope = "merchant"
	PlatformScope Scope = "platform"
)

// Target is resolved by OpenRails once. Its selectors confer no authority.
// AuthorityGroupID is an optional opaque association owned by a configured
// authorization provider; OpenRails does not read that provider's tables.
type Target struct {
	MerchantID billing.MerchantID
	// MerchantSlug is empty when an ID-selected, group-bound directory has no
	// canonical name authority. Authorize by immutable IDs, never a stale name.
	MerchantSlug     string
	AuthorityGroupID string
}

type Requirement struct {
	Permission string
	Scope      Scope
	Target     Target
}

type Authentication interface {
	AuthenticateRequest(context.Context, *http.Request) (Identity, error)
}
type AuthenticationFunc func(context.Context, *http.Request) (Identity, error)

func (f AuthenticationFunc) AuthenticateRequest(ctx context.Context, r *http.Request) (Identity, error) {
	return f(ctx, r)
}

// Authorization evaluates current authority for the exact operation and target.
// It must not expand a verified machine/delegated credential's ceiling.
type Authorization interface {
	Authorize(context.Context, *http.Request, Identity, Requirement) error
}
type AuthorizationFunc func(context.Context, *http.Request, Identity, Requirement) error

func (f AuthorizationFunc) Authorize(ctx context.Context, r *http.Request, i Identity, q Requirement) error {
	return f(ctx, r, i, q)
}

// RecentSignIn checks the request's native user signed in recently enough for
// an operation that moves money or grants access, with helpers/auth
// RecentSignInChecker's errors.
type RecentSignIn interface {
	CheckRecentSignIn(context.Context, *http.Request) error
}
type RecentSignInFunc func(context.Context, *http.Request) error

func (f RecentSignInFunc) CheckRecentSignIn(ctx context.Context, r *http.Request) error {
	return f(ctx, r)
}

// Integration is supplied at construction. Authorization can be omitted only
// when no privileged route is published. Personal customer ownership is checked
// by OpenRails against the explicitly mapped canonical customer and selected merchant.
// Without RecentSignIn, native users are refused the operations that need a
// recent sign-in; NewIntegration derives it from the provider's principal.
type Integration struct {
	Authentication Authentication
	Authorization  Authorization
	RecentSignIn   RecentSignIn
}
