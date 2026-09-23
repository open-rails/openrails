package billingauth

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/pkg/merchant"
)

// PrincipalKind is verified credential provenance, not a role or permission.
type PrincipalKind string

const (
	NativeUser    PrincipalKind = "user"
	Machine       PrincipalKind = "machine"
	DelegatedUser PrincipalKind = "delegated"
)

// Identity is authentication output. Native users carry no role/permission
// snapshot. Permissions belongs only to verified non-user credential ceilings.
type Identity struct {
	Kind      PrincipalKind
	SubjectID string
	// CustomerID is the canonical payable UUID explicitly mapped by the host
	// from (Issuer, SubjectID). Required for customer/checkout and personal catalog operations, optional
	// for merchant staff. OpenRails never guesses or hashes this mapping.
	CustomerID      string
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
	CustomerScope Scope = "customer"
	PlatformScope Scope = "platform"
)

// Target is resolved by OpenRails once. Its selectors confer no authority.
// AuthorityGroupID is an optional opaque association owned by a configured
// authorization provider; OpenRails does not read that provider's tables.
type Target struct {
	MerchantID merchant.ID
	// MerchantSlug is empty when an ID-selected, group-bound directory has no
	// canonical name authority. Authorize by immutable IDs, never a stale name.
	MerchantSlug     string
	CustomerID       string
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

// Integration is supplied at construction. Authorization can be omitted only
// when no privileged route is published. Personal customer ownership is checked
// by OpenRails against the explicitly mapped canonical customer and selected merchant.
type Integration struct {
	Authentication Authentication
	Authorization  Authorization
}
