package server

import (
	"context"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/operator"
)

// Control-plane operations, for hosted products: merchant provisioning, the
// AuthKit scope each merchant is, fleet aggregates and retirement. Who a
// request is, and what it may do, is AuthKit's: AuthKit().Authenticator().

// ProvisionMerchant returns the merchant a name resolves to, or creates one
// claiming it, bound to a new merchant permission group owned by
// req.OwnerUserID (none: the operator's merchant). A user claim answers to
// Config.MerchantCreation. It is how a merchant is created outside the
// manifest; no OpenRails route creates one.
func (s *Server) ProvisionMerchant(ctx context.Context, req billing.ProvisionMerchantParams) (*billing.ProvisionMerchantResult, error) {
	return operator.ProvisionMerchant(ctx, s.cp, req)
}

// ListMerchantsForSubject returns the live merchants where subject is a
// customer.
func (s *Server) ListMerchantsForSubject(ctx context.Context, subject string) ([]billing.MerchantRef, error) {
	return operator.ListMerchantsForSubject(ctx, s.cp, subject)
}

// ListUserMerchants returns the live merchants the server's user userID
// holds a role in, with that role and its permissions: a hosted product's
// merchant list for a user AuthKit authenticated. Customer relationships are
// a separate listing.
func (s *Server) ListUserMerchants(ctx context.Context, userID string) ([]billing.UserMerchant, error) {
	return s.cp.ListUserMerchants(ctx, userID)
}

// ListActiveMerchantIDs pages the live merchants, newest first, for host
// background work.
func (s *Server) ListActiveMerchantIDs(ctx context.Context, page billing.PageRequest) (*billing.ListPage[billing.MerchantID], error) {
	return operator.ListActiveMerchantIDs(ctx, s.cp, page)
}

// AuthKit's built-in permissions over the roles a merchant defines from
// billing.Permissions' merchant: names (its persona's CustomRoles), held by
// the owner: reading what each role grants, and creating, changing and
// deleting its custom roles. The routes' own permissions are
// billing.Permissions.
const (
	MerchantRolesRead   = "merchant:roles:read"
	MerchantRolesManage = "merchant:roles:manage"
)

// MerchantRoles are the roles the server declares for a merchant's team
// members and API keys, least privilege first: viewer
// (billing.MerchantBillingRead), support (and MerchantBillingManage), owner
// (every merchant: permission). A merchant adds its own (MerchantRole).
func MerchantRoles() []iam.Role { return controlplane.MerchantRoles() }

// MerchantRole is the merchant role named name: a declared one ("viewer"), or
// a custom role ("custom-storefront", merchant:custom-storefront), which
// AuthKit looks up in the merchant's group wherever it is used.
func MerchantRole(name string) (iam.Role, bool) { return controlplane.MerchantRole(name) }

// MerchantScope is where merchant id's staff and credentials hold their
// merchant: permissions: its AuthKit group, whose id is the merchant's. A
// hosted product asks a request's AuthKit Verified
// Can(scope, billing.MerchantBillingRead) there.
// billing.ErrMerchantUnresolved when the merchant is not live.
func (s *Server) MerchantScope(ctx context.Context, id billing.MerchantID) (auth.Scope, error) {
	return s.cp.MerchantScope(ctx, id)
}

// MerchantByName is the live merchant a current or former name resolves to,
// with its current name, without an authority check.
// billing.ErrMerchantUnresolved for none.
func (s *Server) MerchantByName(ctx context.Context, name string) (billing.MerchantID, string, error) {
	return s.cp.MerchantByName(ctx, name)
}

// EnsureCustomerPermissionGroup idempotently creates the customer's portal
// group (its ID is customerID) owned by ownerSubject and returns its ID.
func (s *Server) EnsureCustomerPermissionGroup(ctx context.Context, customerID, ownerSubject string) (string, error) {
	return s.cp.EnsureCustomerPermissionGroup(ctx, customerID, ownerSubject)
}

// SubjectHasVaultedPaymentMethod reports whether subject has a usable vaulted
// payment method with vaultMerchant; a host implements
// Deps.HasVaultedPaymentMethod with it.
func (s *Server) SubjectHasVaultedPaymentMethod(ctx context.Context, vaultMerchant billing.MerchantID, subject string) (bool, error) {
	return operator.SubjectHasVaultedPaymentMethod(ctx, s.cp, vaultMerchant, subject)
}

// FleetAnalytics returns cross-merchant aggregates over the last windowDays
// (1..365; anything else is billing.ErrInvalid), excluding one merchant. The
// caller gates and audits it.
func (s *Server) FleetAnalytics(ctx context.Context, exclude billing.MerchantID, windowDays int) (*billing.FleetSnapshot, error) {
	return operator.FleetAnalytics(ctx, s.cp, exclude, windowDays)
}

// FleetTimeseries returns the weekly fleet trend over 4..52 weeks (anything
// else is billing.ErrInvalid), excluding one merchant. The caller gates and
// audits it.
func (s *Server) FleetTimeseries(ctx context.Context, exclude billing.MerchantID, weeks int) (*billing.FleetSeries, error) {
	return operator.FleetTimeseries(ctx, s.cp, exclude, weeks)
}

// ListMerchantRetirementCandidates pages unreserved live merchants with their
// activity facts; the host owns the dormancy policy over them.
func (s *Server) ListMerchantRetirementCandidates(ctx context.Context, req billing.MerchantRetirementCandidateListParams) (billing.MerchantRetirementCandidatePage, error) {
	return operator.ListMerchantRetirementCandidates(ctx, s.cp, req)
}

// RetireUnusedMerchant retires an inactive merchant still bound to groupID and
// releases its group; refusals are reported in the result.
func (s *Server) RetireUnusedMerchant(ctx context.Context, id billing.MerchantID, groupID string) (billing.MerchantRetirement, error) {
	return operator.RetireUnusedMerchant(ctx, s.cp, id, groupID)
}

// CompletePendingMerchantRetirements retries group releases of committed
// retirements, up to limit, and reports how many completed.
func (s *Server) CompletePendingMerchantRetirements(ctx context.Context, limit int) (int, error) {
	return operator.CompletePendingMerchantRetirements(ctx, s.cp, limit)
}

// ConsoleClientID is the admin console's OAuth client at the server's
// AuthKit, which the server declares: a public client exchanging a person's
// session token for one bound to the merchant they act on (RFC 8693, the
// merchant's id as group_id), with a DPoP proof. Absent with
// Config.ConsoleIssuer.
const ConsoleClientID = controlplane.ConsoleClientID
