package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/open-rails/authkit/iam"
	helpersauth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/staffperm"
)

// Control-plane operations, for hosted products: merchant provisioning and
// names, the merchant directory, fleet aggregates and retirement.

// AuthenticateUser verifies a session of the server's own accounts on r.
func (s *Server) AuthenticateUser(r *http.Request) (openrails.Identity, error) {
	authenticator := s.cp.UserAuthenticator()
	if authenticator == nil {
		return openrails.Identity{}, openrails.ErrUnauthenticated
	}
	user, err := authenticator.Authenticate(r.Context(), r)
	if err != nil {
		return openrails.Identity{}, err
	}
	return s.identity(user.UserID, user.SessionID, user.Email, user.Username, user.EmailVerified)
}

// ProvisionMerchant returns the merchant a name resolves to, or creates one
// claiming it, bound to a new merchant permission group owned by
// req.OwnerUserID. A user claim answers to Config.MerchantCreation. A
// merchant's own changes afterwards (its name, display name, API host) go
// through its routes.
func (s *Server) ProvisionMerchant(ctx context.Context, req billing.ProvisionMerchantParams) (*billing.ProvisionMerchantResult, error) {
	return operator.ProvisionMerchant(ctx, s.cp, req)
}

// SetMerchantAPIHost binds the host name requests resolve to this merchant
// from, as the operator and without the DNS proof SetAPIHost asks of a
// merchant: a host of the deployment's own. Empty clears it.
func (s *Server) SetMerchantAPIHost(ctx context.Context, id billing.MerchantID, apiHost string) error {
	return operator.SetMerchantAPIHost(ctx, s.cp, id, apiHost)
}

// ListMerchantsForSubject returns the live merchants where subject is a
// customer.
func (s *Server) ListMerchantsForSubject(ctx context.Context, subject string) ([]billing.MerchantRef, error) {
	return operator.ListMerchantsForSubject(ctx, s.cp, subject)
}

// ListUserMerchants returns the live merchants the user authenticated by r
// holds a staff or owner role in. It checks the sign-in is still active and
// reads current memberships; customer relationships are a separate listing.
func (s *Server) ListUserMerchants(ctx context.Context, r *http.Request) ([]billing.UserMerchant, error) {
	if r == nil {
		return nil, openrails.ErrUnauthenticated
	}
	core := s.cp.Core()
	claims, err := core.VerifyRequest(r.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	if !claims.IsUser() || claims.IsResourceToken() {
		return nil, openrails.ErrUnauthenticated
	}
	// Listing memberships takes a user ID, unlike Can's session-bound identity.
	// Check the verified session explicitly before crossing that boundary.
	if err := core.CheckSession(ctx, claims); err != nil {
		if errors.Is(err, iam.ErrSessionRevoked) {
			err = errors.Join(err, helpersauth.ErrRevoked)
		}
		return nil, err
	}
	return s.cp.ListUserMerchants(ctx, claims.UserID)
}

// ListActiveMerchantIDs pages the live merchants, newest first, for host
// background work.
func (s *Server) ListActiveMerchantIDs(ctx context.Context, page billing.PageRequest) (*billing.ListPage[billing.MerchantID], error) {
	return operator.ListActiveMerchantIDs(ctx, s.cp, page)
}

// The permissions the server's merchant persona declares: its staff routes'
// guards (openrails.StaffReads, StaffWrites, MerchantConfig). Owners hold all
// three, support MerchantRead and MerchantWrite, viewers MerchantRead.
const (
	MerchantRead  = staffperm.Read
	MerchantWrite = staffperm.Write
	MerchantAdmin = staffperm.Admin
)

// ResolveAuthorizedMerchant captures the merchant behind ref (the user's sole
// merchant when empty), then checks live that the user r authenticates as
// holds permission on it (MerchantRead, MerchantWrite or MerchantAdmin). The
// slug is display metadata; carry the ID.
func (s *Server) ResolveAuthorizedMerchant(ctx context.Context, r *http.Request, ref, permission string) (billing.MerchantID, string, error) {
	return s.cp.ResolveAuthorizedMerchant(ctx, r, ref, permission)
}

// ResolveMerchantForGroup captures the merchant ID and canonical slug behind a
// group reference, without an authority check.
func (s *Server) ResolveMerchantForGroup(ctx context.Context, ref string) (billing.MerchantID, string, error) {
	return s.cp.ResolveMerchantForGroup(ctx, ref)
}

// HasRootPermission checks live whether the user r authenticates as holds
// permission in the root group.
func (s *Server) HasRootPermission(ctx context.Context, r *http.Request, permission string) (bool, error) {
	return s.cp.HasRootPermission(ctx, r, permission)
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
