//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/server"
	"github.com/stretchr/testify/require"
)

// The route MembersAdmin calls creates the registration invitation. Its
// emailed bearer code enrolls one identity into only its stored group/role;
// the delivery email is not an identity binding for a registration code.
func TestServerConsoleInvitedStaffEnrollment(t *testing.T) {
	f := newFixture(t)
	messages := &authtest.Outbox{}
	srv := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		standaloneConsole("http://127.0.0.1/"+f.schema, nil)(cfg, deps)
		cfg.Auth.Registration.AllowMissingSenders = false
		cfg.Auth.Registration.Verification = "" // exercise the server's required-verification default
		// This workflow tests invitations, not the independently tested cooldown.
		cfg.Auth.HTTP.RateLimits = map[string]authkit.RateLimit{"invite_create": {Limit: 20, Window: time.Hour}}
		deps.Auth.Email = messages.Email()
	})
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	ownerID, owner := newOwner(t, srv)
	merchant := provisionOwned(t, srv, uniqueName("invited"), ownerID)
	other := provision(t, srv, uniqueName("not-invited"))
	api := "/" + f.schema + "/v1"
	groups := api + "/groups/" + merchant.String()
	require.Equal(t, http.StatusNotFound, call(t, handler, owner, http.MethodGet, api+"/admin/users", nil).Code, "enrollment adds no site-wide admin surface")

	// A staff member may invite only a role within their own permissions.
	created := call(t, handler, owner, http.MethodPost, groups+"/roles", map[string]any{
		"name": "limited-inviter", "permissions": []string{
			"merchant:members:read", "merchant:members:manage",
			"merchant:customer:read", "merchant:catalog:read", "merchant:config:read",
			"merchant:metrics:read", "merchant:usage:read",
		},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	role := decode[iam.GroupRole](t, created).Name
	inviter := newAccount(t, srv)
	granted := call(t, handler, owner, http.MethodPut, groups+"/members/users/"+inviter.ID, map[string]any{"role": role.String()})
	require.Equal(t, http.StatusOK, granted.Code, granted.Body.String())
	inviterToken := authtest.SignIn(t, srv.AuthKit(), inviter).AccessToken
	denied := call(t, handler, inviterToken, http.MethodPost, groups+"/invitations", map[string]any{"email": uniqueName("escalation") + "@e2e.test", "role": "merchant:owner"})
	require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	require.Contains(t, denied.Body.String(), "role_assignment_escalation")

	invite := func(email string, expires *time.Time) string {
		t.Helper()
		result := call(t, handler, inviterToken, http.MethodPost, groups+"/invitations", map[string]any{"email": email, "role": "merchant:viewer", "expires_at": expires})
		require.Equal(t, http.StatusAccepted, result.Code, result.Body.String())
		link, err := url.Parse(messages.Last(t, iam.MessageInvite, email).Link)
		require.NoError(t, err)
		require.Equal(t, "/admin/invite", link.Path)
		require.Empty(t, link.RawQuery, "the invite code is in a fragment, never a server URL")
		fragment, err := url.ParseQuery(link.Fragment)
		require.NoError(t, err)
		code := fragment.Get("invite_code")
		require.NotEmpty(t, code)
		return code
	}
	attempts := 0
	post := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, api+path, bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		attempts++
		request.RemoteAddr = "198.51.100." + strconv.Itoa(attempts) + ":1234"
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		return result
	}
	register := func(email, code string) *httptest.ResponseRecorder {
		return post("/register", map[string]any{"identifier": email, "username": "staff" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12], "password": authtest.Password, "invite_code": code})
	}
	for name, code := range map[string]string{"missing": "", "invalid": "not-an-invitation"} {
		result := register(uniqueName(name)+"@e2e.test", code)
		require.Equal(t, http.StatusForbidden, result.Code, name)
	}
	deliveredTo := uniqueName("delivery") + "@e2e.test"
	code := invite(deliveredTo, nil)
	// Registration codes are deliberately bearer capabilities. AuthKit binds
	// the stored role to the identity that proves its chosen contact.
	redeemer := uniqueName("redeemer") + "@e2e.test"
	result := register(redeemer, code)
	require.Equal(t, http.StatusAccepted, result.Code, result.Body.String())
	verification := messages.Last(t, iam.MessageVerification, redeemer)
	require.Equal(t, iam.PurposeSignup, verification.Purpose)
	verified := post("/verify/confirm", map[string]string{"identifier": redeemer, "code": verification.Code})
	require.Equal(t, http.StatusOK, verified.Code, verified.Body.String())
	tokens := authtest.SignIn(t, srv.AuthKit(), authtest.User{Email: redeemer, Password: authtest.Password})
	who := decode[struct {
		ID string `json:"id"`
	}](t, call(t, handler, tokens.AccessToken, http.MethodGet, api+"/me", nil))
	roles, err := srv.AuthKit().GroupRoles(t.Context(), iam.GroupByID(merchant.String()), []iam.Subject{iam.UserSubject(who.ID)})
	require.NoError(t, err)
	require.Equal(t, "viewer", roles[iam.UserSubject(who.ID)].Name())
	absent, err := srv.AuthKit().GroupRoles(t.Context(), iam.GroupByID(other.String()), []iam.Subject{iam.UserSubject(who.ID)})
	require.NoError(t, err)
	require.Empty(t, absent)
	replay := register(uniqueName("replay")+"@e2e.test", code)
	require.Equal(t, http.StatusForbidden, replay.Code)

	expires := time.Now().UTC().Add(5 * time.Second)
	expired := invite(uniqueName("expires")+"@e2e.test", &expires)
	timer := time.NewTimer(time.Until(expires) + 20*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	refused := register(uniqueName("expired")+"@e2e.test", expired)
	require.Equal(t, http.StatusForbidden, refused.Code)
}
