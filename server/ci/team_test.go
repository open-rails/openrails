//go:build e2e && integration

package ci_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/server"
)

// SEC: adding a teammate by email (the server's InviteMerchantTeamMember)
// never grants a merchant role to an account that has not proved the
// address: anyone can register an address they do not own. Only a live account that verified it joins directly; an unverified or
// deleted account gets the answer an unregistered address gets (an invitation
// where registration mints one), and no role.
func TestSecurityTeamEmailGrantsOnlyAVerifiedAccount(t *testing.T) {
	f := newFixture(t)
	for _, mode := range []iam.RegistrationMode{iam.RegistrationModeClosed, iam.RegistrationModeOpen} {
		registers := mode != iam.RegistrationModeClosed
		t.Run(string(mode), func(t *testing.T) {
			ctx := t.Context()
			slug := "team-" + uuid.NewString()[:8]
			mail := &outbox{}
			cp := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
				cfg.Registration = mode
				cfg.Auth.Issuer = "http://127.0.0.1/" + slug
				if registers {
					deps.Engine.Email = mail
				}
			})
			require.NoError(t, engine.Graph(cp.Client()).Runtime.InitRiver(ctx), "bind job producers, as the standalone boot does")
			core := cp.AuthKit()
			account := func(verified bool) iam.User {
				id := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
				u, err := core.CreateUser(ctx, iam.NewUser{Email: "team-" + id + "@e2e.test", Username: "team_" + id, EmailVerified: verified})
				require.NoError(t, err)
				return u
			}
			id := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
			email := "owner-" + id + "@e2e.test"
			owner, err := core.CreateUser(ctx, iam.NewUser{Email: email, Username: "owner_" + id, Password: authtest.Password, EmailVerified: true})
			require.NoError(t, err)
			m, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: slug, OwnerUserID: owner.ID})
			require.NoError(t, err)
			token := authtest.SignIn(t, core, authtest.User{User: owner, Email: email, Password: authtest.Password}).AccessToken
			by := userActor(t, cp, token)

			invite := func(email string) (*billing.TeamInviteResult, error) {
				return cp.InviteMerchantTeamMember(ctx, by, m.MerchantID, billing.InviteTeamMemberParams{Email: email, Role: "viewer"})
			}
			// shape is what the answer reveals: fields and refusal.
			shape := func(res *billing.TeamInviteResult, err error) []any {
				if err != nil {
					return []any{errors.Is(err, server.ErrTeamInvitesDisabled)}
				}
				return []any{res.Member != nil, res.Invite != nil, res.URL != nil}
			}
			onTeam := func(u iam.User) bool {
				team, err := cp.ListMerchantTeam(ctx, m.MerchantID)
				require.NoError(t, err)
				for _, member := range team {
					if member.UserID == u.ID {
						return true
					}
				}
				return false
			}

			unknown := shape(invite("team-" + uuid.NewString()[:12] + "@e2e.test"))
			require.Equal(t, registers, cp.TeamInvitesEnabled())
			if registers {
				require.Equal(t, []any{false, true, true}, unknown, "an unregistered address gets a link")
			} else {
				require.Equal(t, []any{true}, unknown, "closed registration mints no link")
			}

			for what, u := range map[string]iam.User{"unverified": account(false), "deleted": account(true)} {
				if what == "deleted" {
					results, err := core.DeleteUsers(ctx, iam.SystemIdentity(), []string{u.ID})
					require.NoError(t, err)
					require.NoError(t, results[0].Err)
				}
				require.Equal(t, unknown, shape(invite(*u.Email)), "%s: answered like an unregistered address", what)
				require.False(t, onTeam(u), "%s: an account that has not proved the address holds no merchant role", what)
			}

			verified := account(true)
			res, err := invite(strings.ToUpper(*verified.Email))
			require.NoError(t, err)
			require.NotNil(t, res.Member, "added at once: %+v", res)
			require.True(t, onTeam(verified), "control: the account that proved the address joins")

			// The team keeps an owner; the role a member holds changes.
			member, err := cp.SetMerchantTeamRole(ctx, by, m.MerchantID, verified.ID, "support")
			require.NoError(t, err)
			require.Equal(t, "support", member.Role)
			_, err = cp.SetMerchantTeamRole(ctx, by, m.MerchantID, owner.ID, "viewer")
			require.ErrorIs(t, err, server.ErrLastOwner)
			require.ErrorIs(t, cp.RemoveMerchantTeamMember(ctx, by, m.MerchantID, owner.ID), server.ErrLastOwner)
			require.NoError(t, cp.RemoveMerchantTeamMember(ctx, by, m.MerchantID, verified.ID))
			require.False(t, onTeam(verified))

			if registers {
				// The control plane's mail reaches the engine's sender, rendered,
				// from the deployment's own address.
				require.NoError(t, core.ResetAccountMFA(ctx, verified.ID))
				notice := mail.to(*verified.Email)
				require.NotNil(t, notice, "the notice was sent")
				require.Empty(t, notice.From)
				require.Contains(t, notice.Subject, "Two-step verification")
			}
		})
	}
}

// outbox is an EmailSender that keeps what it is given.
type outbox struct {
	mu   sync.Mutex
	sent []openrails.Email
}

func (o *outbox) Send(_ context.Context, m openrails.Email) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sent = append(o.sent, m)
	return nil
}

func (*outbox) CheckHealth(context.Context) error { return nil }

func (o *outbox) to(address string) *openrails.Email {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := range o.sent {
		if o.sent[i].To == address {
			return &o.sent[i]
		}
	}
	return nil
}
