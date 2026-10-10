package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	helpersauth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// A hosted product's user model on the server's accounts: a merchant's team,
// API keys and federated grants. OpenRails serves none of it over HTTP; the
// product's routes authorize the caller (ResolveAuthorizedMerchant) and call
// these. Roles are the merchant roles: owner, support and viewer.

// Actor is who changes a merchant's team or API keys.
type Actor struct {
	identity helpersauth.Identity
	// bounded: the actor's own permissions, not AuthKit, bound what it grants.
	bounded     bool
	permissions []string
}

// OperatorActor is the deployment's operator: AuthKit checks no authority.
func OperatorActor() Actor { return Actor{identity: iam.SystemIdentity()} }

// UserActor is the user r authenticates as by a session of the server's
// accounts. AuthKit refuses what their own role does not cover
// (iam.ErrInsufficientAuthority, iam.ErrRoleAssignmentEscalation) and their
// revoked session (iam.ErrSessionRevoked).
func (s *Server) UserActor(r *http.Request) (Actor, error) {
	who, err := s.cp.RequestIdentity(r)
	if err != nil {
		return Actor{}, err
	}
	return Actor{identity: who}, nil
}

// CheckRecentSignIn is AuthKit's step-up check for the user r authenticates
// as, before a sensitive change (an API key, a role): nil when they signed in
// recently enough, else helpers/auth's ErrStepUpRequired with AuthKit's
// step-up error, ErrRevoked for a revoked session.
func (s *Server) CheckRecentSignIn(ctx context.Context, r *http.Request) error {
	return s.cp.CheckRecentSignIn(ctx, r)
}

// CredentialActor is a credential that carries its own permissions, such as
// an API key or a trusted issuer's token: it grants no role its permissions
// do not cover (ErrRoleEscalation).
func CredentialActor(permissions []string) Actor {
	return Actor{identity: iam.SystemIdentity(), bounded: true, permissions: append([]string(nil), permissions...)}
}

// grants checks that a role is within the actor's own permissions.
func (s *Server) grants(by Actor, role iam.Role) error {
	if !by.bounded {
		return nil
	}
	covered, err := s.cp.RoleCoveredBy(role, by.permissions)
	if err != nil {
		return err
	}
	if !covered {
		return ErrRoleEscalation
	}
	return nil
}

func merchantRole(name string) (iam.Role, error) {
	role, ok := controlplane.MerchantRole(strings.TrimSpace(name))
	if !ok {
		return iam.Role{}, fmt.Errorf("%w: %q", ErrUnknownMerchantRole, name)
	}
	return role, nil
}

// ListMerchantTeam is the merchant's team, owners first.
func (s *Server) ListMerchantTeam(ctx context.Context, id billing.MerchantID) ([]billing.TeamMember, error) {
	return s.cp.ListMerchantTeam(ctx, id)
}

// ListMerchantTeamInvites is the merchant's invitation links (pending,
// redeemed, revoked), never their codes.
func (s *Server) ListMerchantTeamInvites(ctx context.Context, id billing.MerchantID) ([]billing.TeamInvite, error) {
	return s.cp.ListMerchantTeamInvites(ctx, id)
}

// TeamInvitesEnabled reports whether an invitation can register a new
// account: Config.Registration admits it.
func (s *Server) TeamInvitesEnabled() bool { return s.cp.InvitesEnabled() }

// InviteMerchantTeamMember adds the account that verified req.Email at once
// (Member), or makes a single-use link that registers its holder and adds
// them (Invite and URL), where TeamInvitesEnabled; otherwise
// ErrTeamInvitesDisabled.
func (s *Server) InviteMerchantTeamMember(ctx context.Context, by Actor, id billing.MerchantID, req billing.InviteTeamMemberParams) (*billing.TeamInviteResult, error) {
	email := strings.TrimSpace(req.Email)
	if email == "" {
		return nil, fmt.Errorf("%w: email is required", billing.ErrInvalid)
	}
	role, err := merchantRole(req.Role)
	if err != nil {
		return nil, err
	}
	if err := s.grants(by, role); err != nil {
		return nil, err
	}
	result, err := s.cp.InviteMerchantTeamMember(ctx, id, email, role, by.identity)
	if errors.Is(err, iam.ErrExternalInvitesDisabled) {
		err = errors.Join(ErrTeamInvitesDisabled, err)
	}
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// RevokeMerchantTeamInvite revokes one of the merchant's invitation links;
// billing.ErrNotFound when it has none with that id.
func (s *Server) RevokeMerchantTeamInvite(ctx context.Context, by Actor, id billing.MerchantID, inviteID string) error {
	revoked, err := s.cp.RevokeMerchantTeamInvite(ctx, id, strings.TrimSpace(inviteID), by.identity)
	if err == nil && !revoked {
		err = fmt.Errorf("%w: team invite %q", billing.ErrNotFound, inviteID)
	}
	return err
}

// SetMerchantTeamRole sets a member's role. Demoting the last owner is
// ErrLastOwner, a user outside the team ErrNotTeamMember.
func (s *Server) SetMerchantTeamRole(ctx context.Context, by Actor, id billing.MerchantID, userID, roleName string) (*billing.TeamMember, error) {
	role, err := merchantRole(roleName)
	if err != nil {
		return nil, err
	}
	if err := s.grants(by, role); err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if err := s.cp.ChangeMerchantTeamRole(ctx, id, userID, role, by.identity); err != nil {
		return nil, err
	}
	team, err := s.cp.ListMerchantTeam(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, m := range team {
		if m.UserID == userID {
			return &m, nil
		}
	}
	return nil, ErrNotTeamMember
}

// RemoveMerchantTeamMember removes a member. Removing the last owner is
// ErrLastOwner, a user outside the team ErrNotTeamMember.
func (s *Server) RemoveMerchantTeamMember(ctx context.Context, by Actor, id billing.MerchantID, userID string) error {
	return s.cp.RemoveMerchantTeamMember(ctx, id, strings.TrimSpace(userID), by.identity)
}

// CreateMerchantAPIKey mints a key holding a merchant role. Its secret is in
// the result only: it is never stored and cannot be read again.
func (s *Server) CreateMerchantAPIKey(ctx context.Context, by Actor, id billing.MerchantID, req billing.CreateAPIKeyParams) (*billing.CreatedAPIKey, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 120 {
		return nil, fmt.Errorf("%w: an API key's name is 1 to 120 characters", billing.ErrInvalid)
	}
	role, err := merchantRole(req.Role)
	if err != nil {
		return nil, err
	}
	if err := s.grants(by, role); err != nil {
		return nil, err
	}
	key, secret, err := s.cp.MintMerchantAPIKey(ctx, id, name, role, by.identity)
	if err != nil {
		return nil, err
	}
	return &billing.CreatedAPIKey{APIKey: key, Secret: secret}, nil
}

// ListMerchantAPIKeys is every key of the merchant (live, expired and
// revoked), without secrets.
func (s *Server) ListMerchantAPIKeys(ctx context.Context, id billing.MerchantID) ([]billing.APIKey, error) {
	return s.cp.ListMerchantAPIKeys(ctx, id)
}

// RevokeMerchantAPIKey revokes one of the merchant's keys; billing.ErrNotFound
// when it has none with that id.
func (s *Server) RevokeMerchantAPIKey(ctx context.Context, by Actor, id billing.MerchantID, keyID string) error {
	keyID = strings.TrimSpace(keyID)
	if _, err := uuid.Parse(keyID); err != nil {
		return fmt.Errorf("%w: API key %q", billing.ErrNotFound, keyID)
	}
	revoked, err := s.cp.RevokeMerchantAPIKey(ctx, id, keyID, by.identity)
	if err == nil && !revoked {
		err = fmt.Errorf("%w: API key %q", billing.ErrNotFound, keyID)
	}
	return err
}

// ListFederatedGrants is the merchant's federated grants: roles granted by
// email to users of its trusted issuers, pending until accepted.
func (s *Server) ListFederatedGrants(ctx context.Context, id billing.MerchantID) ([]billing.FederatedGrant, error) {
	return s.cp.ListFederatedGrants(ctx, id)
}

// CreateFederatedGrant invites an email with a merchant role: a user of an
// issuer trusted for the merchant accepts it with that verified email
// (AcceptFederatedGrant). A malformed email is ErrFederatedGrantInvalidEmail,
// one already invited ErrFederatedGrantExists.
func (s *Server) CreateFederatedGrant(ctx context.Context, by Actor, id billing.MerchantID, req billing.CreateFederatedGrantParams) (*billing.FederatedGrant, error) {
	role, err := merchantRole(req.Role)
	if err != nil {
		return nil, err
	}
	if err := s.grants(by, role); err != nil {
		return nil, err
	}
	grant, err := s.cp.CreateFederatedGrant(ctx, id, req.Email, role.Name())
	if err != nil {
		return nil, err
	}
	return &grant, nil
}

// RevokeFederatedGrant ends a grant, pending or accepted;
// ErrFederatedGrantNotFound when the merchant has none with that id.
func (s *Server) RevokeFederatedGrant(ctx context.Context, by Actor, id billing.MerchantID, grantID billing.FederatedGrantID) error {
	grant, err := s.cp.FederatedGrant(ctx, id, grantID)
	if err != nil {
		return err
	}
	role, _ := controlplane.MerchantRole(grant.Role)
	if err := s.grants(by, role); err != nil {
		return err
	}
	return s.cp.RevokeFederatedGrant(ctx, id, grantID)
}

// ListFederatedInvites is the pending grants the user r authenticates as (a
// trusted issuer's access token) may accept with its verified email.
func (s *Server) ListFederatedInvites(ctx context.Context, r *http.Request) ([]billing.FederatedInvite, error) {
	user, err := s.cp.ResolveResourceUser(r.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	return s.cp.PendingFederatedGrants(ctx, user)
}

// AcceptFederatedGrant binds a pending grant to the user r authenticates as
// (a trusted issuer's access token carrying the invited, verified email):
// ErrFederatedGrantUnverified without one, ErrFederatedGrantExists when the
// user already holds a grant on the merchant.
func (s *Server) AcceptFederatedGrant(ctx context.Context, r *http.Request, grantID billing.FederatedGrantID) (*billing.UserMerchant, error) {
	user, err := s.cp.ResolveResourceUser(r.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	m, err := s.cp.AcceptFederatedGrant(ctx, user, grantID)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// Refusal is how a hosted product answers a credential the server refused:
// the status, the error code and the headers a client retries with
// (WWW-Authenticate, DPoP-Nonce).
type Refusal struct {
	Status  int
	Code    string
	Headers map[string]string
}

// CredentialRefusal is the Refusal for an error ListUserMerchants,
// ListFederatedInvites, AcceptFederatedGrant or UserActor answered because of
// the request's credential; false for any other error.
func CredentialRefusal(err error) (Refusal, bool) {
	var challenge credential.ChallengeError
	var gate billingauth.GateError
	switch {
	case errors.As(err, &challenge), errors.Is(err, credential.ErrResourceTokenInvalid), errors.Is(err, credential.ErrResourceTokenIssuerUnknown),
		errors.Is(err, credential.ErrResourceTokenMerchantNotBound), errors.Is(err, credential.ErrResourceTokenUnavailable), errors.Is(err, credential.ErrResourceServerNotConfigured):
		gate = credential.ResourceTokenRefusal(err)
	case errors.Is(err, helpersauth.ErrRevoked), errors.Is(err, iam.ErrSessionRevoked):
		gate = billingauth.Refusal(billing.CodeCredentialRevoked)
	case errors.Is(err, billingauth.ErrUnauthenticated):
		gate = billingauth.Refusal(billing.CodeAuthenticationRequired)
	default:
		return Refusal{}, false
	}
	return Refusal{Status: gate.Status, Code: gate.Code, Headers: gate.Headers}, true
}
