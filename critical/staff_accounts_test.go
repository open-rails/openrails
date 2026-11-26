//go:build integration

package critical_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/helpers/smtp/smtptest"
	"github.com/stretchr/testify/require"
)

// criticalPublicURL is the binary's public_url in these scenarios: AuthKit's
// issuer, the URL DPoP proofs name, and the origin of the links it logs and
// sends.
const criticalPublicURL = "https://billing.critical.invalid"

// operatorRole is a root role short of the owner's: an operator of the
// merchant directory.
const operatorRole = "root:merchant-directory-admin"

// The standalone server's own accounts begin with a setup link the server
// logs, and grow by invitation from the console's Team page.
func TestStaffAccounts(t *testing.T) {
	t.Run("OR-J01-S34/a-fresh-server-logs-one-setup-link-that-creates-the-owner-once", func(t *testing.T) {
		s := startStaffServer(t, newWorld(t))
		links := s.setupLinks()
		require.Len(t, links, 1, "a fresh server logs one setup link: %s", s.log)
		token := linkFragment(t, links[0], "/admin/setup", "token")
		groups := s.routeGroups(t)
		for _, group := range []string{"groups", "invitations", "registration", "setup"} {
			require.Contains(t, groups, group, "the console's setup, invitation and Team pages have their routes")
		}

		owner := s.browser(t)
		status, raw := owner.setup(t, token, "owner@critical.invalid", "owner")
		require.Equal(t, http.StatusOK, status, string(raw))
		session, ownerID := owner.enroll(t, raw)
		status, raw = owner.call(t, http.MethodGet, "/auth/v1/groups/root/members", session, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), ownerID)
		require.Contains(t, string(raw), "root:owner")
		require.True(t, owner.consoleRoot(t, session), "the owner is the console's operator")

		// Spent, the link creates nobody else.
		status, raw = s.browser(t).setup(t, token, "second@critical.invalid", "second")
		require.Equal(t, http.StatusNotFound, status, string(raw))
		require.Contains(t, string(raw), "setup_link_invalid")
		status, raw = owner.call(t, http.MethodGet, "/auth/v1/groups/root/members", session, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var members struct{ Data []json.RawMessage }
		require.NoError(t, json.Unmarshal(raw, &members))
		require.Len(t, members.Data, 1, "root holds the owner alone: %s", raw)
		require.Len(t, s.setupLinks(), 1)
	})

	t.Run("OR-J01-S35/setup-link-issues-a-break-glass-owner-link-while-operators-exist", func(t *testing.T) {
		w := newWorld(t)
		keys := staffKeys(t)
		// An init job seeded an operator, so the server announces no link.
		manifest := filepath.Join(t.TempDir(), "bootstrap.yaml")
		require.NoError(t, os.WriteFile(manifest, []byte(`users:
  - email: ops@critical.invalid
    username: ops
    email_verified: true
    password: { plaintext: "Seeded-operator-1", enforce: true }
    root_role: root:owner
`), 0o600))
		out, err := staffCLI(t, w, keys, nil, "push-auth-bootstrap", "--file", manifest).CombinedOutput()
		require.NoError(t, err, "the operator is seeded: %s", out)
		s := startStaffServerWithKeys(t, w, keys)
		require.Empty(t, s.setupLinks(), "root holds an operator: no link is announced: %s", s.log)

		issue := func() string {
			cmd := staffCLI(t, w, keys, nil, "setup-link")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			require.NoError(t, cmd.Run(), stderr.String())
			require.Contains(t, stderr.String(), "single use")
			return linkFragment(t, strings.TrimSpace(stdout.String()), "/admin/setup", "token")
		}
		replaced, token := issue(), issue()
		status, raw := s.browser(t).setup(t, replaced, "replaced@critical.invalid", "replaced")
		require.Equal(t, http.StatusNotFound, status, "a newer link replaces the live one: %s", raw)
		require.Contains(t, string(raw), "setup_link_invalid")

		owner := s.browser(t)
		status, raw = owner.setup(t, token, "breakglass@critical.invalid", "breakglass")
		require.Equal(t, http.StatusOK, status, "the break-glass link works while operators exist: %s", raw)
		session, _ := owner.enroll(t, raw)
		require.True(t, owner.consoleRoot(t, session))
		status, raw = s.browser(t).setup(t, token, "again@critical.invalid", "again")
		require.Equal(t, http.StatusNotFound, status, string(raw))
		require.Contains(t, string(raw), "setup_link_invalid")

		// Without the console nothing opens a link: the command refuses.
		out, err = staffCLI(t, w, keys, map[string]any{"admin_console": map[string]any{"disabled": true}}, "setup-link").CombinedOutput()
		require.Error(t, err, string(out))
		require.Contains(t, string(out), "admin_console.disabled")
		require.NotContains(t, string(out), "#token=")
	})

	t.Run("OR-J01-S36/without-email-an-operators-invitation-answers-its-link-to-share", func(t *testing.T) {
		s := startStaffServer(t, newWorld(t))
		owner, session := s.owner(t)
		// The Team page's invitation by email: none is sent, so its link is
		// the inviter's to share.
		status, raw := owner.call(t, http.MethodPost, "/auth/v1/groups/root/invitations", session, map[string]string{"email": "teammate@critical.invalid", "role": operatorRole})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var created struct {
			Code, URL  string
			Emailed    bool
			Invitation struct{ Role, Email string }
		}
		require.NoError(t, json.Unmarshal(raw, &created))
		require.False(t, created.Emailed, string(raw))
		require.Equal(t, operatorRole, created.Invitation.Role)
		require.Equal(t, "teammate@critical.invalid", created.Invitation.Email)
		require.Equal(t, created.Code, linkFragment(t, created.URL, "/admin/invite", "invite_code"))
		// An invite link, for an account that holds one, carries its code
		// as link_code.
		status, raw = owner.call(t, http.MethodPost, "/auth/v1/groups/root/invitations", session, map[string]string{"role": operatorRole})
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &created))
		require.Equal(t, created.Code, linkFragment(t, created.URL, "/admin/invite", "link_code"))
		status, raw = owner.call(t, http.MethodGet, "/auth/v1/groups/root/invitations", session, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Equal(t, 2, strings.Count(string(raw), `"role":"`+operatorRole+`"`), string(raw))
	})

	t.Run("OR-J01-S37/an-accepted-invitation-makes-the-invitee-an-operator", func(t *testing.T) {
		mail := smtptest.Start(t, smtptest.Options{})
		s := startStaffServer(t, newWorld(t), map[string]any{"email_smtp": map[string]any{"host": mail.Host, "port": mail.Port, "from": "ops@critical.invalid"}})
		owner, session := s.owner(t)
		const email = "teammate@critical.invalid"
		status, raw := owner.call(t, http.MethodPost, "/auth/v1/groups/root/invitations", session, map[string]string{"email": email, "role": operatorRole})
		require.Equal(t, http.StatusAccepted, status, "an emailed invitation answers nothing to share: %s", raw)
		link := regexp.MustCompile(`https://\S+/admin/invite#\S+`).FindString(mailTo(t, mail, email, "invited").Text)
		code := linkFragment(t, link, "/admin/invite", "invite_code")

		// The invitee signs up with it, proves the address and enrolls a
		// second factor: an operator from the first request on.
		invitee := s.browser(t)
		status, raw = invitee.call(t, http.MethodPost, "/auth/v1/register", "", map[string]string{"identifier": email, "username": "teammate", "password": authtest.Password, "invite_code": code})
		require.Equal(t, http.StatusAccepted, status, string(raw))
		var pending struct{ Challenge string }
		require.NoError(t, json.Unmarshal(raw, &pending))
		verify := regexp.MustCompile(`Code: (\S+)`).FindStringSubmatch(mailTo(t, mail, email, "Verify").Text)
		require.Len(t, verify, 2)
		status, raw = invitee.call(t, http.MethodPost, "/auth/v1/verify/confirm", "", map[string]string{"challenge": pending.Challenge, "code": verify[1]})
		require.Equal(t, http.StatusOK, status, string(raw))
		mate, mateID := invitee.enroll(t, raw)
		require.True(t, invitee.consoleRoot(t, mate), "the invitee is an operator")
		status, raw = owner.call(t, http.MethodGet, "/auth/v1/groups/root/members", session, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), mateID)
		require.Contains(t, string(raw), operatorRole)
	})

	t.Run("OR-J01-S38/a-server-whose-staff-sign-in-elsewhere-advertises-no-groups-or-setup", func(t *testing.T) {
		// A hosted product whose staff sign in elsewhere mounts sign-in alone:
		// nothing offers the first owner's setup, invitations or a Team.
		elsewhere := newStandaloneWorld(t, func(c map[string]any) { c["Console"], c["StaffElsewhere"] = true, true })
		groups := fixtureRouteGroups(t, elsewhere)
		for _, group := range []string{"groups", "invitations", "setup"} {
			require.NotContains(t, groups, group)
		}
		status, raw := elsewhere.request(t, http.MethodPost, "/auth/v1/setup", "", map[string]string{"token": "x"})
		require.Equal(t, http.StatusNotFound, status, string(raw))
		status, raw = elsewhere.request(t, http.MethodGet, "/auth/v1/groups/root/members", elsewhere.AliceToken, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))

		// Without the console no page opens a setup link, so none is mounted;
		// the server's own accounts still have their groups.
		headless := newStandaloneWorld(t)
		groups = fixtureRouteGroups(t, headless)
		require.Contains(t, groups, "groups")
		require.NotContains(t, groups, "setup")
	})
}

// staffServer is the openrails binary serving a world's database.
type staffServer struct {
	base string
	log  *lockedBuffer
}

// staffKeys is a directory holding AuthKit's signing key and totp.key.
func staffKeys(t *testing.T) string {
	t.Helper()
	keys := t.TempDir()
	writeSigningKeys(t, keys)
	writeTOTPKey(t, keys)
	return keys
}

// staffCLI is an openrails command on w's database and keys, its
// configuration extended by extra.
func staffCLI(t *testing.T, w *world, keys string, extra map[string]any, args ...string) *exec.Cmd {
	t.Helper()
	settings := map[string]any{"public_url": criticalPublicURL, "auth": map[string]any{"keys": map[string]any{"path": keys}}}
	for key, value := range extra {
		settings[key] = value
	}
	return hostCLICommand(t, w, settings, args...)
}

func startStaffServer(t *testing.T, w *world, extra ...map[string]any) *staffServer {
	t.Helper()
	return startStaffServerWithKeys(t, w, staffKeys(t), extra...)
}

// startStaffServerWithKeys runs run-server and waits until its background
// work, AuthKit's included, runs.
func startStaffServerWithKeys(t *testing.T, w *world, keys string, extra ...map[string]any) *staffServer {
	t.Helper()
	base, log, _ := runServer(t, w, keys, extra...)
	require.Eventually(t, func() bool { return strings.Contains(log.String(), "OpenRails background work running") }, 90*time.Second, 200*time.Millisecond, "the server starts its background work: %s", log)
	return &staffServer{base: base, log: log}
}

// setupLinks are the first owner's setup links the server logged.
func (s *staffServer) setupLinks() []string {
	var out []string
	for line := range strings.SplitSeq(s.log.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		msg, _ := record["msg"].(string)
		link, _ := record["url"].(string)
		if strings.Contains(msg, "no administrator yet") && link != "" {
			out = append(out, link)
		}
	}
	return out
}

// routeGroups are the AuthKit route groups the server advertises.
func (s *staffServer) routeGroups(t *testing.T) []string {
	t.Helper()
	status, raw := s.browser(t).call(t, http.MethodGet, "/auth/v1/capabilities", "", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var caps struct {
		RouteGroups []string `json:"route_groups"`
	}
	require.NoError(t, json.Unmarshal(raw, &caps))
	return caps.RouteGroups
}

func fixtureRouteGroups(t *testing.T, w *standaloneWorld) []string {
	t.Helper()
	status, raw := w.request(t, http.MethodGet, "/auth/v1/capabilities", "", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var caps struct {
		RouteGroups []string `json:"route_groups"`
	}
	require.NoError(t, json.Unmarshal(raw, &caps))
	require.NotEmpty(t, caps.RouteGroups, string(raw))
	return caps.RouteGroups
}

// owner creates the owner with the link the server logged and signs them in.
func (s *staffServer) owner(t *testing.T) (*staffBrowser, string) {
	t.Helper()
	links := s.setupLinks()
	require.Len(t, links, 1, "a fresh server logs one setup link: %s", s.log)
	b := s.browser(t)
	status, raw := b.setup(t, linkFragment(t, links[0], "/admin/setup", "token"), "owner@critical.invalid", "owner")
	require.Equal(t, http.StatusOK, status, string(raw))
	session, _ := b.enroll(t, raw)
	return b, session
}

// staffBrowser is a person's browser at the server: one DPoP key, which
// every sign-in binds its session to.
type staffBrowser struct {
	s   *staffServer
	key *authtest.DPoPKey
}

func (s *staffServer) browser(t *testing.T) *staffBrowser {
	return &staffBrowser{s: s, key: authtest.NewDPoPKey(t)}
}

// call sends body to path with a fresh proof of the key, bound to token when
// one is given, and once more with the server's DPoP nonce when it asks.
func (b *staffBrowser) call(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	nonce := ""
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(t.Context(), method, b.s.base+path, bytes.NewReader(payload))
		require.NoError(t, err)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		// A proof names the URL clients reach: the public one.
		target, _, _ := strings.Cut(path, "?")
		req.Header.Set("DPoP", b.key.Proof(t, method, criticalPublicURL+target, token, nonce))
		if token != "" {
			req.Header.Set("Authorization", "DPoP "+token)
		}
		response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		require.NoError(t, err)
		raw, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		require.NoError(t, err)
		if next := response.Header.Get("DPoP-Nonce"); attempt == 0 && response.StatusCode == http.StatusUnauthorized && next != "" {
			nonce = next
			continue
		}
		return response.StatusCode, raw
	}
}

// setup spends a setup link's token on an owner account, as the console's
// /admin/setup page does.
func (b *staffBrowser) setup(t *testing.T, token, email, username string) (int, []byte) {
	t.Helper()
	return b.call(t, http.MethodPost, "/auth/v1/setup", "", map[string]string{"token": token, "email": email, "username": username, "password": authtest.Password})
}

// enroll finishes a first sign-in, which the server's accounts start by
// enrolling a second factor: an authenticator app. It returns the session's
// access token and the account's id.
func (b *staffBrowser) enroll(t *testing.T, signIn []byte) (string, string) {
	t.Helper()
	var first struct {
		Status     string
		Enrollment struct {
			TokenSet struct {
				AccessToken string `json:"access_token"`
			} `json:"token_set"`
		}
	}
	require.NoError(t, json.Unmarshal(signIn, &first))
	require.Equal(t, "enrollment_required", first.Status, "a first sign-in enrolls a second factor: %s", signIn)
	enrollment := first.Enrollment.TokenSet.AccessToken
	status, raw := b.call(t, http.MethodPost, "/auth/v1/me/2fa/setup", enrollment, map[string]string{"method": "totp"})
	require.Equal(t, http.StatusOK, status, string(raw))
	var totp struct{ Secret string }
	require.NoError(t, json.Unmarshal(raw, &totp))
	status, raw = b.call(t, http.MethodPost, "/auth/v1/me/2fa/factors", enrollment, map[string]string{"method": "totp", "code": authtest.TOTPCode(t, totp.Secret, time.Now())})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var created struct {
		Auth struct {
			Status   string
			TokenSet struct {
				AccessToken string `json:"access_token"`
			} `json:"token_set"`
			User struct{ ID string }
		}
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	require.Equal(t, "complete", created.Auth.Status, string(raw))
	require.NotEmpty(t, created.Auth.TokenSet.AccessToken, string(raw))
	return created.Auth.TokenSet.AccessToken, created.Auth.User.ID
}

// consoleRoot reports whether the console's merchant directory reads the
// session as a root operator's.
func (b *staffBrowser) consoleRoot(t *testing.T, session string) bool {
	t.Helper()
	status, raw := b.call(t, http.MethodGet, "/admin/merchants", session, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var page struct{ Root bool }
	require.NoError(t, json.Unmarshal(raw, &page))
	return page.Root
}

// linkFragment is the fragment parameter name of link, a console page at
// path beneath the public URL; the code never reaches a server.
func linkFragment(t *testing.T, link, path, name string) string {
	t.Helper()
	u, err := url.Parse(link)
	require.NoError(t, err, link)
	require.Equal(t, criticalPublicURL+path, u.Scheme+"://"+u.Host+u.Path, link)
	require.Empty(t, u.RawQuery, link)
	q, err := url.ParseQuery(u.Fragment)
	require.NoError(t, err, link)
	require.NotEmpty(t, q.Get(name), "%s carries no %s", link, name)
	return q.Get(name)
}

// mailTo is the newest message to address whose subject names about.
func mailTo(t *testing.T, mail *smtptest.Server, address, about string) smtptest.Message {
	t.Helper()
	var found smtptest.Message
	require.Eventually(t, func() bool {
		for _, m := range slices.Backward(mail.Messages()) {
			if slices.Contains(m.To, address) && strings.Contains(m.Subject, about) {
				found = m
				return true
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond, "a message to %s about %q", address, about)
	return found
}
