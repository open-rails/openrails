//go:build greenfield && integration

package ci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/standalonedb"
)

// SEC: adding a teammate by email never grants a merchant role to an account
// that has not proved the address: anyone can register an address they do not
// own. Only a live account that verified it joins directly; an unverified or
// deleted account gets the answer an unregistered address gets (an invitation
// where the posture mints one), and no role.
func TestSecurityTeamEmailGrantsOnlyAVerifiedAccount(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, standalonedb.ApplyAuthKit(t.Context(), f.pool))
	for _, hosted := range []bool{false, true} {
		name := "standalone"
		if hosted {
			name = "hosted"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			slug := "team-" + uuid.NewString()[:8]
			rt, err := embed.New(ctx, embed.Options{
				Config: &config.Config{
					TestMode:          config.CredentialPostureSandbox,
					ProviderWriteMode: config.ProviderWriteModeReadOnly,
					DB:                &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
					ReturnOrigins:     []string{"https://greenfield.test"},
				},
				PGXPool: f.pool,
				River:   embed.RiverManagedByOpenRails(f.schema),
			})
			require.NoError(t, err)
			t.Cleanup(func() { _ = rt.Close(context.Background()) })
			opts := controlplane.Options{Auth: &hostconfig.AuthConfig{
				Issuer: "http://127.0.0.1/" + slug, KeysPath: t.TempDir(), AllowMemory: true, AllowMissingSenders: true, AllowEphemeralSigningKey: true, AllowLoopbackHTTP: true, DirectPeerIP: true,
			}}
			if hosted {
				opts.HostedPosture, opts.EmailSender = true, new(authtest.Outbox).Email()
			}
			cp, err := controlplane.Attach(ctx, rt, opts)
			require.NoError(t, err)
			require.NoError(t, app.HostGraph(rt).Runtime.InitRiver(ctx), "bind job producers, as the standalone boot does")
			core := cp.Core()
			account := func(verified bool) iam.User {
				id := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
				u, err := core.CreateUser(ctx, iam.NewUser{Email: "team-" + id + "@greenfield.test", Username: "team_" + id, EmailVerified: verified})
				require.NoError(t, err)
				return u
			}
			id := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
			email := "owner-" + id + "@greenfield.test"
			owner, err := core.CreateUser(ctx, iam.NewUser{Email: email, Username: "owner_" + id, Password: authtest.Password, EmailVerified: true})
			require.NoError(t, err)
			_, err = cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: slug, OwnerUserID: owner.ID})
			require.NoError(t, err)
			handler, err := cp.Handler()
			require.NoError(t, err)
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			// Inviting needs a recent sign-in, which a minted token is not.
			token := authtest.SignIn(t, core, authtest.User{User: owner, Email: email, Password: authtest.Password}).AccessToken

			call := func(method, path string, body any) (int, map[string]any) {
				var data io.Reader
				if body != nil {
					raw, err := json.Marshal(body)
					require.NoError(t, err)
					data = bytes.NewReader(raw)
				}
				req, err := http.NewRequestWithContext(ctx, method, server.URL+"/v1/merchant/team"+path, data)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("X-OpenRails-Merchant", slug)
				req.Header.Set("Content-Type", "application/json")
				res, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				defer res.Body.Close()
				out := map[string]any{}
				require.NoError(t, json.NewDecoder(res.Body).Decode(&out))
				return res.StatusCode, out
			}
			invite := func(email string) (int, map[string]any) {
				return call(http.MethodPost, "/invites", map[string]string{"email": email, "role": "viewer"})
			}
			// shape is what the answer reveals: status, fields and error code.
			shape := func(status int, body map[string]any) []any {
				out := []any{status, body["added"], body["member"] != nil, body["invite"] != nil, body["url"] != nil}
				if e, ok := body["error"].(map[string]any); ok {
					out = append(out, e["code"])
				}
				return out
			}
			onTeam := func(u iam.User) bool {
				status, body := call(http.MethodGet, "", nil)
				require.Equal(t, http.StatusOK, status, "%v", body)
				for _, m := range body["data"].([]any) {
					if m.(map[string]any)["user_id"] == u.ID {
						return true
					}
				}
				return false
			}

			unknown := shape(invite("team-" + uuid.NewString()[:12] + "@greenfield.test"))
			for what, u := range map[string]iam.User{"unverified": account(false), "deleted": account(true)} {
				if what == "deleted" {
					results, err := core.DeleteUsers(ctx, iam.SystemActor(), []string{u.ID})
					require.NoError(t, err)
					require.NoError(t, results[0].Err)
				}
				require.Equal(t, unknown, shape(invite(*u.Email)), "%s: answered like an unregistered address", what)
				require.False(t, onTeam(u), "%s: an account that has not proved the address holds no merchant role", what)
			}

			verified := account(true)
			status, body := invite(strings.ToUpper(*verified.Email))
			require.Equal(t, http.StatusCreated, status, "%v", body)
			require.Equal(t, true, body["added"])
			require.True(t, onTeam(verified), "control: the account that proved the address joins")
		})
	}
}
