package controlplane

// Merchant team management (#760): the control-plane surface behind
// /v1/merchant/team. Roster, invites, role changes, and removal all go through
// AuthKit CORE group-membership calls (ListGroupMembers / AssignGroupRoleAs /
// UnassignGroupRoleAs / RemoveGroupSubjectAs / CreateGroupInviteLink) — never
// raw AuthKit SQL (bootstrap.go doctrine). Members hold one of the FIXED merchant
// catalog roles (#567): owner / support / viewer.
//
// Two facts shape this surface, both grounded in what AuthKit v0.82 exposes to a
// consumer:
//
//   - The known-user "consent invite" flow (#147/#193 group_membership_invites)
//     is NOT reachable via the embedded client. The only invite primitive a
//     merchant owner can create+list+revoke is the group invite link. So an
//     invite to an ALREADY-REGISTERED email is a direct role assignment (the
//     invitee is added immediately); an invite to an UNREGISTERED email mints a
//     single-use register+join link the owner shares (fail-soft copy-link).
//
//   - Invite-link minting requires AuthKit's registration to be open/invite-only
//     (ExternalInvitesEnabled). Locked-down standalone runs registration CLOSED,
//     so the unregistered-email path returns ErrTeamInvitesDisabled there: new
//     teammates must be provisioned by the operator first, then added by email.
//     Hosted/embedded-open postures mint the link. This is the documented
//     "register-then-accept hosted / operator-provisioned embedded" split.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/pkg/merchant"
)

var (
	// ErrCannotRemoveLastOwner guards the #760 invariant: a merchant must always
	// retain at least one (human) owner. Demoting or removing the last owner is
	// refused with a corrective error, never silently allowed.
	ErrCannotRemoveLastOwner = errors.New("controlplane: cannot remove or demote the last merchant owner")

	// ErrNotATeamMember is returned by role-change/remove for a user who holds no
	// role in the merchant group (so there is nothing to change or remove).
	ErrNotATeamMember = errors.New("controlplane: user is not a member of this merchant")

	// ErrTeamInvitesDisabled is returned when inviting an UNREGISTERED email but
	// the deployment runs AuthKit registration closed (locked-down standalone):
	// no self-registration link can be minted. The operator must provision the
	// account first; then it can be added by email as an existing user.
	ErrTeamInvitesDisabled = errors.New("controlplane: link invites for new users are disabled on this deployment")
)

// MerchantTeamMember is a human member of a merchant's team: an AuthKit user
// holding a fixed catalog role in the merchant permission-group. Display fields
// are hydrated best-effort; a member with no stored email/username still lists
// (identified by user id).
type MerchantTeamMember struct {
	UserID   string `json:"user_id"`
	Email    string `json:"email,omitempty"`
	Username string `json:"username,omitempty"`
	Role     string `json:"role"`
}

// MerchantTeamInvite is a pending register+join invite link for the merchant.
// The single-use code/URL is returned ONLY at creation (in InviteResult) — it
// is never listed, exactly like an API-key secret.
type MerchantTeamInvite struct {
	ID         string     `json:"id"`
	Role       string     `json:"role"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RedeemedAt *time.Time `json:"redeemed_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// MerchantTeamInviteResult is the outcome of inviting an email. Exactly one of
// Member (the email was an existing user, added to the team immediately) or
// Invite+URL (a single-use link the owner shares with a new user) is set.
type MerchantTeamInviteResult struct {
	// Added is true when the email resolved to an existing user who was added to
	// the team directly (no link needed).
	Added bool `json:"added"`
	// Member is set when Added: the member now on the team.
	Member *MerchantTeamMember `json:"member,omitempty"`
	// Invite is set when a link was minted for an unregistered email.
	Invite *MerchantTeamInvite `json:"invite,omitempty"`
	// URL is the single-use register+join link — shown once, here, only when a
	// link was minted. The owner shares it with the invitee.
	URL string `json:"url,omitempty"`
}

// ListMerchantTeam returns the merchant's human team: every user-kind member of
// the merchant permission-group with their catalog role, display fields hydrated
// best-effort. The synthetic bootstrap api-key actor (a non-login system owner
// seeded in admin-less deployments) is excluded — it is not a teammate.
func (c *ControlPlane) ListMerchantTeam(ctx context.Context, mid merchant.ID) ([]MerchantTeamMember, error) {
	if c == nil || c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	ctx, slug, err := c.merchantGroupScopeForID(ctx, mid)
	if err != nil {
		return nil, err
	}
	members, err := c.humanTeam(ctx, slug)
	if err != nil {
		return nil, err
	}
	out := make([]MerchantTeamMember, 0, len(members))
	for _, m := range members {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			// owner first, then support, then viewer (reverse of least-privilege).
			return teamRoleRank(out[i].Role) < teamRoleRank(out[j].Role)
		}
		return teamMemberLabel(out[i]) < teamMemberLabel(out[j])
	})
	return out, nil
}

// InviteMerchantTeamMember adds a teammate by email. If the email belongs to an
// existing user, that user is assigned role immediately (Added). Otherwise a
// single-use register+join link is minted and returned (URL) — unless the
// deployment runs registration closed, in which case ErrTeamInvitesDisabled is
// returned. role must be a fixed catalog role. actorUserID is the acting AuthKit
// user (empty for non-user principals → genesis owner actor; the CALLER must
// have enforced owner authority via the route gate + no-escalation).
func (c *ControlPlane) InviteMerchantTeamMember(ctx context.Context, mid merchant.ID, email, role, actorUserID string) (MerchantTeamInviteResult, error) {
	if c == nil || c.Core() == nil {
		return MerchantTeamInviteResult{}, ErrNoControlPlane
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if _, ok := MerchantRolePermissions(role); !ok {
		return MerchantTeamInviteResult{}, ErrUnknownMerchantRole
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return MerchantTeamInviteResult{}, fmt.Errorf("controlplane: invite email is required")
	}
	ctx, slug, err := c.merchantGroupScopeForID(ctx, mid)
	if err != nil {
		return MerchantTeamInviteResult{}, err
	}
	actor, err := c.resolveTeamActor(ctx, slug, actorUserID)
	if err != nil {
		return MerchantTeamInviteResult{}, err
	}

	// Existing user? Add them directly — no registration needed.
	user, err := c.Core().GetUserByEmail(ctx, email)
	switch {
	case err == nil && user != nil:
		if aerr := c.Core().AssignGroupRoleAs(ctx, actor, MerchantGroup(slug), authkit.UserSubject(user.ID), authkit.Role(role)); aerr != nil {
			return MerchantTeamInviteResult{}, aerr
		}
		return MerchantTeamInviteResult{Added: true, Member: &MerchantTeamMember{
			UserID:   user.ID,
			Email:    derefString(user.Email),
			Username: derefString(user.Username),
			Role:     role,
		}}, nil
	case err != nil && !isUserNotFound(err):
		return MerchantTeamInviteResult{}, fmt.Errorf("controlplane: resolve invite email: %w", err)
	}

	// Unregistered email: mint a single-use register+join link (if the posture
	// permits self-registration). AuthKit gates minting on the same members:manage
	// no-escalation the route gate + caller already enforced.
	if !c.Core().ExternalInvitesEnabled() {
		return MerchantTeamInviteResult{}, ErrTeamInvitesDisabled
	}
	link, err := c.Core().CreateGroupInviteLink(ctx, authkit.CreateGroupInviteLinkRequest{
		Persona:      MerchantType,
		InstanceSlug: slug,
		Role:         authkit.Role(role),
		InvitedBy:    actor,
	})
	if err != nil {
		if errors.Is(err, authkit.ErrExternalInvitesDisabled) {
			return MerchantTeamInviteResult{}, ErrTeamInvitesDisabled
		}
		return MerchantTeamInviteResult{}, err
	}
	return MerchantTeamInviteResult{
		Invite: &MerchantTeamInvite{ID: link.ID, Role: role},
		URL:    link.URL,
	}, nil
}

// ListMerchantTeamInvites returns the merchant's invite links (pending, redeemed,
// and revoked — status is the audit view), NEVER the code/URL.
func (c *ControlPlane) ListMerchantTeamInvites(ctx context.Context, mid merchant.ID) ([]MerchantTeamInvite, error) {
	if c == nil || c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	ctx, slug, err := c.merchantGroupScopeForID(ctx, mid)
	if err != nil {
		return nil, err
	}
	links, err := c.Core().ListGroupInviteLinks(ctx, MerchantGroup(slug))
	if err != nil {
		return nil, err
	}
	out := make([]MerchantTeamInvite, 0, len(links))
	for _, l := range links {
		out = append(out, MerchantTeamInvite{
			ID:         l.ID,
			Role:       string(l.Role),
			CreatedAt:  l.CreatedAt,
			ExpiresAt:  l.ExpiresAt,
			RedeemedAt: l.RedeemedAt,
			RevokedAt:  l.RevokedAt,
		})
	}
	return out, nil
}

// InvitesEnabled reports whether the deployment can mint register+join links for
// unregistered emails (the console tailors its invite affordance on this).
func (c *ControlPlane) InvitesEnabled() bool {
	return c != nil && c.Core() != nil && c.Core().ExternalInvitesEnabled()
}

// RevokeMerchantTeamInvite revokes a pending invite link by id, scoped to the
// merchant's group. Returns false when no such live link exists in this merchant.
func (c *ControlPlane) RevokeMerchantTeamInvite(ctx context.Context, mid merchant.ID, linkID string) (bool, error) {
	if c == nil || c.Core() == nil {
		return false, ErrNoControlPlane
	}
	ctx, slug, err := c.merchantGroupScopeForID(ctx, mid)
	if err != nil {
		return false, err
	}
	err = c.Core().RevokeGroupInviteLink(ctx, MerchantGroup(slug), strings.TrimSpace(linkID))
	if err != nil {
		if errors.Is(err, authkit.ErrInviteLinkNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ChangeMerchantTeamRole sets targetUserID's role to exactly newRole (fixed
// catalog). The last-owner invariant is enforced up front: demoting the sole
// (human) owner is refused with ErrCannotRemoveLastOwner. Self-demotion is
// therefore allowed only when another owner exists. actorUserID as in Invite.
func (c *ControlPlane) ChangeMerchantTeamRole(ctx context.Context, mid merchant.ID, targetUserID, newRole, actorUserID string) error {
	if c == nil || c.Core() == nil {
		return ErrNoControlPlane
	}
	newRole = strings.ToLower(strings.TrimSpace(newRole))
	if _, ok := MerchantRolePermissions(newRole); !ok {
		return ErrUnknownMerchantRole
	}
	targetUserID = strings.TrimSpace(targetUserID)
	if targetUserID == "" {
		return ErrNotATeamMember
	}
	ctx, slug, err := c.merchantGroupScopeForID(ctx, mid)
	if err != nil {
		return err
	}
	actor, err := c.resolveTeamActor(ctx, slug, actorUserID)
	if err != nil {
		return err
	}

	members, err := c.humanTeam(ctx, slug)
	if err != nil {
		return err
	}
	current, ok := members[targetUserID]
	if !ok {
		return ErrNotATeamMember
	}
	if current.Role == newRole {
		return nil // no-op
	}
	if current.Role == MerchantRoleOwner && newRole != MerchantRoleOwner && ownerCount(members) <= 1 {
		return ErrCannotRemoveLastOwner
	}

	// Assign the new role first, then strip the old — order keeps a promotion
	// from ever transiently dropping a role. The pre-check above already guards
	// the last-owner case; AuthKit's own refuseIfLastOwner is a backstop.
	if err := c.Core().AssignGroupRoleAs(ctx, actor, MerchantGroup(slug), authkit.UserSubject(targetUserID), authkit.Role(newRole)); err != nil {
		return err
	}
	if err := c.Core().UnassignGroupRoleAs(ctx, actor, MerchantGroup(slug), authkit.UserSubject(targetUserID), authkit.Role(current.Role)); err != nil {
		if errors.Is(err, authkit.ErrCannotRemoveLastAdminRole) {
			return ErrCannotRemoveLastOwner
		}
		return err
	}
	return nil
}

// RemoveMerchantTeamMember removes targetUserID from the merchant team entirely.
// Refuses to remove the sole (human) owner (ErrCannotRemoveLastOwner). Removing a
// non-member is ErrNotATeamMember.
func (c *ControlPlane) RemoveMerchantTeamMember(ctx context.Context, mid merchant.ID, targetUserID, actorUserID string) error {
	if c == nil || c.Core() == nil {
		return ErrNoControlPlane
	}
	targetUserID = strings.TrimSpace(targetUserID)
	if targetUserID == "" {
		return ErrNotATeamMember
	}
	ctx, slug, err := c.merchantGroupScopeForID(ctx, mid)
	if err != nil {
		return err
	}
	actor, err := c.resolveTeamActor(ctx, slug, actorUserID)
	if err != nil {
		return err
	}
	members, err := c.humanTeam(ctx, slug)
	if err != nil {
		return err
	}
	current, ok := members[targetUserID]
	if !ok {
		return ErrNotATeamMember
	}
	if current.Role == MerchantRoleOwner && ownerCount(members) <= 1 {
		return ErrCannotRemoveLastOwner
	}
	if err := c.Core().RemoveGroupSubjectAs(ctx, actor, MerchantGroup(slug), authkit.UserSubject(targetUserID)); err != nil {
		if errors.Is(err, authkit.ErrCannotRemoveLastAdminRole) {
			return ErrCannotRemoveLastOwner
		}
		return err
	}
	return nil
}

// humanTeam returns the merchant's user-kind members keyed by user id, with
// display fields hydrated and the synthetic bootstrap api-key actor excluded.
// Each member's Role is their highest-privilege catalog role (a subject may hold
// several role rows; the console models one role per member).
func (c *ControlPlane) humanTeam(ctx context.Context, slug string) (map[string]MerchantTeamMember, error) {
	raw, err := c.Core().ListGroupMembers(ctx, MerchantGroup(slug))
	if err != nil {
		return nil, err
	}
	roles := make(map[string]string, len(raw))
	ids := make([]string, 0, len(raw))
	for _, m := range raw {
		if m.SubjectKind != authkit.SubjectKindUser {
			continue
		}
		if existing, seen := roles[m.SubjectID]; !seen || teamRoleRank(string(m.Role)) < teamRoleRank(existing) {
			if !seen {
				ids = append(ids, m.SubjectID)
			}
			roles[m.SubjectID] = string(m.Role)
		}
	}
	refs, err := c.Core().UsersByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]MerchantTeamMember, len(ids))
	for _, id := range ids {
		ref := refs[id]
		if isBootstrapActor(ref.Username, ref.Email) {
			continue
		}
		out[id] = MerchantTeamMember{
			UserID:   id,
			Email:    ref.Email,
			Username: ref.Username,
			Role:     roles[id],
		}
	}
	return out, nil
}

// resolveTeamActor returns the acting AuthKit user for a group mutation: the
// caller's user id when present, else the merchant's genesis owner actor (the
// operator-CLI idiom shared with #757 api-key minting). The genesis actor holds
// merchant:*, so AuthKit's no-escalation check passes; owner authority itself is
// enforced by the route gate + the handler's no-escalation coverage check.
func (c *ControlPlane) resolveTeamActor(ctx context.Context, slug, actorUserID string) (string, error) {
	if actorUserID = strings.TrimSpace(actorUserID); actorUserID != "" {
		return actorUserID, nil
	}
	return c.ensureMerchantAPIKeyActor(ctx, slug)
}

func ownerCount(members map[string]MerchantTeamMember) int {
	n := 0
	for _, m := range members {
		if m.Role == MerchantRoleOwner {
			n++
		}
	}
	return n
}

// isBootstrapActor reports whether a member is the synthetic bootstrap api-key
// actor (a non-login system owner seeded in admin-less deployments), which must
// never appear in the team roster nor count toward the human owner total.
func isBootstrapActor(username, email string) bool {
	return username == bootstrapAPIKeyActorUsername || strings.EqualFold(email, bootstrapAPIKeyActorEmail)
}

func teamRoleRank(role string) int {
	switch role {
	case MerchantRoleOwner:
		return 0
	case MerchantRoleSupport:
		return 1
	case MerchantRoleViewer:
		return 2
	default:
		return 3
	}
}

func teamMemberLabel(m MerchantTeamMember) string {
	if m.Email != "" {
		return m.Email
	}
	if m.Username != "" {
		return m.Username
	}
	return m.UserID
}

func isUserNotFound(err error) bool {
	return errors.Is(err, authkit.ErrUserNotFound) || errors.Is(err, pgx.ErrNoRows)
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
