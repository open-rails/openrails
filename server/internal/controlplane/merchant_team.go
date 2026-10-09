package controlplane

// Merchant team management (#760): the control-plane surface behind
// /v1/merchant/team. Roster, invites, role changes and removal go through
// AuthKit group membership (ListGroupMembers, SetGroupRole, RemoveGroupMember,
// CreateInvitation) — never raw AuthKit SQL. A member holds one of the fixed
// merchant roles (#567).
//
// Adding a teammate by email assigns the role at once only to a live account
// that has VERIFIED the address: anyone can register an address they do not
// own (#1107). Any other address gets a single-use register+join link the
// owner shares — when AuthKit registration is open. Locked-down standalone
// runs registration closed, so there the operator provisions the account and
// verifies its email first (ErrTeamInvitesDisabled).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/open-rails/authkit/iam"
	helpersauth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
)

var (
	// ErrCannotRemoveLastOwner guards the #760 invariant: a merchant always
	// keeps at least one owner. Demoting or removing the last owner is refused
	// with a corrective error, never silently allowed.
	ErrCannotRemoveLastOwner = errors.New("controlplane: cannot remove or demote the last merchant owner")

	// ErrNotATeamMember is returned by role-change/remove for a user who holds no
	// role in the merchant group (so there is nothing to change or remove).
	ErrNotATeamMember = errors.New("controlplane: user is not a member of this merchant")

	// ErrTeamInvitesDisabled is returned when inviting an email no live account
	// has verified but the deployment runs AuthKit registration closed
	// (locked-down standalone): no self-registration link can be minted. The
	// operator must provision the account and verify its email first.
	ErrTeamInvitesDisabled = errors.New("controlplane: link invites for new users are disabled on this deployment")
)

// MerchantTeamMember is a user holding a merchant role. Display fields are
// best-effort; a member with no stored email/username still lists.
type MerchantTeamMember = billing.TeamMember

// MerchantTeamInvite is a register+join invite link for the merchant. Its
// single-use URL is returned ONLY at creation (InviteResult), never listed.
type MerchantTeamInvite = billing.TeamInvite

// MerchantTeamInviteResult is the outcome of inviting an email. Exactly one of
// Member (a live account verified the email and was added immediately) or
// Invite+URL (a single-use link the owner shares with the address) is set.
type MerchantTeamInviteResult = billing.TeamInviteResult

// ListMerchantTeam returns the merchant's team, owners first.
func (c *ControlPlane) ListMerchantTeam(ctx context.Context, mid billing.MerchantID) ([]MerchantTeamMember, error) {
	group, err := c.merchantGroup(ctx, mid)
	if err != nil {
		return nil, err
	}
	members, err := c.team(ctx, group)
	if err != nil {
		return nil, err
	}
	out := make([]MerchantTeamMember, 0, len(members))
	for _, m := range members {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return teamRoleRank(out[i].Role) < teamRoleRank(out[j].Role)
		}
		return teamMemberLabel(out[i]) < teamMemberLabel(out[j])
	})
	return out, nil
}

// InviteMerchantTeamMember adds a teammate by email as actor. If a live
// account has verified the email, it is assigned role immediately (Added).
// Otherwise a single-use register+join link is minted and returned (URL) —
// unless the deployment runs registration closed (ErrTeamInvitesDisabled).
func (c *ControlPlane) InviteMerchantTeamMember(ctx context.Context, mid billing.MerchantID, email string, role iam.Role, actor helpersauth.Identity) (MerchantTeamInviteResult, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return MerchantTeamInviteResult{}, fmt.Errorf("controlplane: invite email is required")
	}
	group, err := c.merchantGroup(ctx, mid)
	if err != nil {
		return MerchantTeamInviteResult{}, err
	}
	user, err := c.client.User(ctx, iam.UserByEmail(email))
	switch {
	case err == nil && user.EmailVerified:
		if _, err := c.client.SetGroupRole(ctx, actor, group, iam.UserSubject(user.ID), role); err != nil {
			return MerchantTeamInviteResult{}, lastOwner(err)
		}
		return MerchantTeamInviteResult{Member: &MerchantTeamMember{
			UserID: user.ID, Email: nonEmpty(text(user.Email)), Username: nonEmpty(user.Username), Role: role.Name(),
		}}, nil
	case err != nil && !errors.Is(err, iam.ErrUserNotFound):
		return MerchantTeamInviteResult{}, fmt.Errorf("controlplane: resolve invite email: %w", err)
	}
	if !c.registers() {
		return MerchantTeamInviteResult{}, ErrTeamInvitesDisabled
	}
	link, err := c.client.CreateInvitation(ctx, actor, group, iam.NewInvitation{Role: role})
	if errors.Is(err, iam.ErrExternalInvitesDisabled) {
		return MerchantTeamInviteResult{}, ErrTeamInvitesDisabled
	}
	if err != nil {
		return MerchantTeamInviteResult{}, err
	}
	invite := teamInvite(link.Invitation)
	return MerchantTeamInviteResult{Invite: &invite, URL: nonEmpty(link.URL)}, nil
}

// ListMerchantTeamInvites returns the merchant's invite links (pending,
// redeemed and revoked — status is the audit view), never their codes.
func (c *ControlPlane) ListMerchantTeamInvites(ctx context.Context, mid billing.MerchantID) ([]MerchantTeamInvite, error) {
	group, err := c.merchantGroup(ctx, mid)
	if err != nil {
		return nil, err
	}
	var out []MerchantTeamInvite
	for inv, err := range iam.All(func(p iam.PageRequest) (iam.ListPage[iam.Invitation], error) {
		return c.client.ListInvitations(ctx, group, p)
	}) {
		if err != nil {
			return nil, err
		}
		out = append(out, teamInvite(inv))
	}
	return out, nil
}

func teamInvite(inv iam.Invitation) MerchantTeamInvite {
	return MerchantTeamInvite{ID: inv.ID, Role: inv.Role.Name(), CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt, RedeemedAt: inv.RedeemedAt, RevokedAt: inv.RevokedAt}
}

// InvitesEnabled reports whether the deployment can mint register+join links
// (the console tailors its invite affordance on this).
func (c *ControlPlane) InvitesEnabled() bool {
	return c != nil && c.Core() != nil && c.registers()
}

// RevokeMerchantTeamInvite revokes an invite link of the merchant as actor.
// It returns false when the merchant has no invite with that id.
func (c *ControlPlane) RevokeMerchantTeamInvite(ctx context.Context, mid billing.MerchantID, id string, actor helpersauth.Identity) (bool, error) {
	group, err := c.merchantGroup(ctx, mid)
	if err != nil {
		return false, err
	}
	err = c.client.RevokeInvitation(ctx, actor, group, strings.TrimSpace(id))
	if errors.Is(err, iam.ErrInvitationNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ChangeMerchantTeamRole makes targetUserID hold newRole, as actor. Demoting
// the last human owner is ErrCannotRemoveLastOwner.
func (c *ControlPlane) ChangeMerchantTeamRole(ctx context.Context, mid billing.MerchantID, targetUserID string, newRole iam.Role, actor helpersauth.Identity) error {
	group, current, owners, err := c.teamMember(ctx, mid, targetUserID)
	if err != nil || current == newRole.Name() {
		return err
	}
	if current == MerchantOwner.Name() && owners <= 1 {
		return ErrCannotRemoveLastOwner
	}
	_, err = c.client.SetGroupRole(ctx, actor, group, iam.UserSubject(strings.TrimSpace(targetUserID)), newRole)
	return lastOwner(err)
}

// RemoveMerchantTeamMember removes targetUserID from the merchant team, as
// actor. Removing the last human owner is ErrCannotRemoveLastOwner.
func (c *ControlPlane) RemoveMerchantTeamMember(ctx context.Context, mid billing.MerchantID, targetUserID string, actor helpersauth.Identity) error {
	group, current, owners, err := c.teamMember(ctx, mid, targetUserID)
	if err != nil {
		return err
	}
	if current == MerchantOwner.Name() && owners <= 1 {
		return ErrCannotRemoveLastOwner
	}
	return lastOwner(c.client.RemoveGroupMember(ctx, actor, group, iam.UserSubject(strings.TrimSpace(targetUserID))))
}

// teamMember is targetUserID's role in the merchant's group and the number of
// users owning it: the merchant keeps a human owner (#760), whatever
// applications also hold the role.
func (c *ControlPlane) teamMember(ctx context.Context, mid billing.MerchantID, targetUserID string) (iam.GroupRef, string, int, error) {
	group, err := c.merchantGroup(ctx, mid)
	if err != nil {
		return iam.GroupRef{}, "", 0, err
	}
	members, err := c.team(ctx, group)
	if err != nil {
		return iam.GroupRef{}, "", 0, err
	}
	target, ok := members[strings.TrimSpace(targetUserID)]
	if !ok {
		return iam.GroupRef{}, "", 0, ErrNotATeamMember
	}
	owners := 0
	for _, m := range members {
		if m.Role == MerchantOwner.Name() {
			owners++
		}
	}
	return group, target.Role, owners, nil
}

// team is the merchant group's users keyed by id, display fields hydrated.
func (c *ControlPlane) team(ctx context.Context, group iam.GroupRef) (map[string]MerchantTeamMember, error) {
	out := map[string]MerchantTeamMember{}
	var ids []string
	for m, err := range iam.All(func(p iam.PageRequest) (iam.ListPage[iam.GroupMember], error) {
		return c.client.ListGroupMembers(ctx, group, iam.MemberQuery{Kinds: []iam.SubjectKind{iam.SubjectKindUser}, Page: p})
	}) {
		if err != nil {
			return nil, err
		}
		out[m.Subject.ID] = MerchantTeamMember{UserID: m.Subject.ID, Role: m.Role.Name()}
		ids = append(ids, m.Subject.ID)
	}
	// The staff roster shows teammates' contact details: the privileged user
	// view, not the public one members carry.
	users, err := c.client.Users(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, u := range users {
		if member, ok := out[id]; ok {
			member.Email, member.Username = nonEmpty(text(u.Email)), nonEmpty(u.Username)
			out[id] = member
		}
	}
	return out, nil
}

// lastOwner maps AuthKit's last-owner refusal onto the team's corrective error.
func lastOwner(err error) error {
	if errors.Is(err, iam.ErrLastOwner) {
		return errors.Join(ErrCannotRemoveLastOwner, err)
	}
	return err
}

// teamRoleRank orders roles most privileged first.
func teamRoleRank(role string) int {
	roles := MerchantRoles()
	for i, r := range roles {
		if r.Name() == role {
			return len(roles) - i
		}
	}
	return len(roles) + 1
}

func teamMemberLabel(m MerchantTeamMember) string {
	switch {
	case m.Email != nil:
		return *m.Email
	case m.Username != nil:
		return *m.Username
	}
	return m.UserID
}

// nonEmpty is &s, nil when empty.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// text is *s, "" when unset.
func text(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
