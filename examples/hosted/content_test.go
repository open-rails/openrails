//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/adapters/smtp"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// A visitor without a key that unlocks a video, signed out included, gets
// 402 with the products on sale that unlock it and its buy page. Buying from
// the server as billing-ui does (a DPoP-bound token for /v1/me, then the
// checkout session) gets the signed video URL: a course for its buyers, every
// video for members.
func TestGatedContent(t *testing.T) {
	app := startStack(t)
	const css, tailwind, qa = "/api/courses/css-101", "/api/courses/tailwind-102", "/api/courses/live-qa"
	// The products on sale that unlock each: the course, the bundle, the membership.
	cssKeys, tailwindKeys, qaKeys := []string{"channel-membership", "course-101", "course-bundle"}, []string{"channel-membership", "course-102", "course-bundle"}, []string{"channel-membership"}

	t.Run("signed out", func(t *testing.T) {
		app.requireBuy(t, "", css, cssKeys, "/courses/css-101/buy")
		app.requireBuy(t, "", qa, qaKeys, "/courses/live-qa/buy")
	})
	t.Run("an unknown course", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, app.get(t, "", "/api/courses/no-such-course").StatusCode)
	})
	t.Run("a course bought alone", func(t *testing.T) {
		alice := app.signUp(t, "alice")
		app.requireBuy(t, alice.session, css, cssKeys, "/courses/css-101/buy")
		app.buy(t, alice, "course-101", "purchase")
		app.requireVideo(t, alice.session, css, "media/courses/css-101.mp4")
		app.requireBuy(t, alice.session, tailwind, tailwindKeys, "/courses/tailwind-102/buy")
		app.requireBuy(t, alice.session, qa, qaKeys, "/courses/live-qa/buy")
	})
	t.Run("a rental", func(t *testing.T) {
		dave := app.signUp(t, "dave")
		app.buy(t, dave, "course-102", "rent")
		app.requireVideo(t, dave.session, tailwind, "media/courses/tailwind-102.mp4")
	})
	t.Run("the bundle", func(t *testing.T) {
		bob := app.signUp(t, "bobby")
		app.buy(t, bob, "course-bundle", "purchase")
		app.requireVideo(t, bob.session, css, "media/courses/css-101.mp4")
		app.requireVideo(t, bob.session, tailwind, "media/courses/tailwind-102.mp4")
		app.requireBuy(t, bob.session, qa, qaKeys, "/courses/live-qa/buy")
	})
	t.Run("a member watches every video", func(t *testing.T) {
		carol := app.signUp(t, "carol")
		app.buy(t, carol, "channel-membership", "monthly")
		app.requireVideo(t, carol.session, css, "media/courses/css-101.mp4")
		app.requireVideo(t, carol.session, tailwind, "media/courses/tailwind-102.mp4")
		app.requireVideo(t, carol.session, qa, "media/courses/live-qa.mp4")
	})
}

// A media URL is good only as signed, and only until it expires.
func TestMediaURLs(t *testing.T) {
	app := startStack(t)
	signed := testMediaKey.url("courses/css-101.mp4")
	require.Equal(t, http.StatusOK, app.get(t, "", signed).StatusCode)

	tampered := strings.Replace(signed, "css-101", "tailwind-102", 1)
	require.Equal(t, http.StatusForbidden, app.get(t, "", tampered).StatusCode)
	past := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	expired := "/media/courses/css-101.mp4?" + url.Values{"expires": {past}, "sig": {testMediaKey.sign("courses/css-101.mp4", past)}}.Encode()
	require.Equal(t, http.StatusForbidden, app.get(t, "", expired).StatusCode)
	require.Equal(t, http.StatusForbidden, app.get(t, "", "/media/courses/css-101.mp4").StatusCode)
	other := mediaKey("another key").url("courses/css-101.mp4")
	require.Equal(t, http.StatusForbidden, app.get(t, "", other).StatusCode)
}

// The course list pages over the host's courses with the server's live
// prices and what the user owns. A members-only video is sold with the
// membership.
func TestCourseList(t *testing.T) {
	app := startStack(t)
	type price struct{ Key, Amount, Currency, Terms string }
	type row struct {
		Slug, Title string
		MembersOnly bool `json:"members_only"`
		Owned       bool
		ProductKey  string `json:"product_key"`
		Prices      []price
	}
	var page struct {
		Data       []row
		NextCursor *string `json:"next_cursor"`
	}
	read := func(token, query string) {
		t.Helper()
		page.Data, page.NextCursor = nil, nil
		app.call(t, token, http.MethodGet, app.URL+"/api/courses?"+query, nil, &page)
	}
	cssPrices := []price{{"rent", "1990000", "USD", "for 3 days"}, {"purchase", "4990000", "USD", "to keep"}}
	membershipPrices := []price{{"monthly", "10000000", "USD", "every 30 days"}, {"yearly", "99000000", "USD", "every 365 days"}}

	read("", "limit=1")
	require.Equal(t, []row{{"css-101", "Intro to CSS", false, false, "course-101", cssPrices}}, page.Data)
	read("", "limit=1&cursor="+*page.NextCursor)
	require.Equal(t, "tailwind-102", page.Data[0].Slug)
	read("", "limit=1&cursor="+*page.NextCursor)
	require.Equal(t, []row{{"live-qa", "Live Q&A", true, false, "channel-membership", membershipPrices}}, page.Data)
	require.Nil(t, page.NextCursor)

	owned := func() []bool {
		var out []bool
		for _, r := range page.Data {
			out = append(out, r.Owned)
		}
		return out
	}
	alice := app.signUp(t, "alice")
	app.buy(t, alice, "course-101", "purchase")
	read(alice.session, "")
	require.Equal(t, []bool{true, false, false}, owned(), "her course only")
	carol := app.signUp(t, "carol")
	app.buy(t, carol, "channel-membership", "yearly")
	read(carol.session, "")
	require.Equal(t, []bool{true, true, true}, owned(), "a member owns every video")

	require.Equal(t, http.StatusBadRequest, app.get(t, "", "/api/courses?limit=0").StatusCode)
}

// The merchant's setup on the platform, exercised: a user who signs up and
// proves their email reaches the platform over SCIM with it, the service
// token reads them, their purchase emails them a receipt, and the PSP's
// signed webhooks are accepted at the merchant's API host while a forged one
// is refused.
func TestPlatformSetup(t *testing.T) {
	app := startStack(t)
	ctx := context.Background()

	// Sign up as the store's sign-in dialog does: register, then the code
	// AuthKit emailed proves the address.
	email := "erin-" + randomHex(4) + "@example.com"
	app.call(t, "", http.MethodPost, app.URL+"/api/v1/register", map[string]string{"identifier": email, "username": "erin_" + randomHex(4), "password": password}, nil)
	sixDigits := regexp.MustCompile(`\b\d{6}\b`)
	code := sixDigits.FindString(app.mailTo(t, email, func(m mail) bool { return sixDigits.MatchString(m.Text) }).Text)
	var signedIn struct {
		TokenSet struct {
			AccessToken string `json:"access_token"`
		} `json:"token_set"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	app.call(t, "", http.MethodPost, app.URL+"/api/v1/verify/confirm", map[string]string{"identifier": email, "code": code}, &signedIn)
	require.NotEmpty(t, signedIn.TokenSet.AccessToken)
	erin, err := billing.ParseCustomerID(signedIn.User.ID)
	require.NoError(t, err)

	// AuthKit's next SCIM push makes her a customer, with her email.
	require.Eventually(t, func() bool {
		c, err := app.bill.GetCustomer(ctx, erin)
		return err == nil && c.Contact != nil && c.Contact.Email != nil && *c.Contact.Email == email
	}, 30*time.Second, 250*time.Millisecond, "the admin customer read shows the email SCIM pushed")

	// Her purchase is receipted to that address.
	app.buy(t, app.customer(t, signedIn.User.ID, signedIn.TokenSet.AccessToken), "course-101", "purchase")
	app.mailTo(t, email, func(m mail) bool { return strings.Contains(m.Text, "4.99 USD") })

	// NMI posts to the merchant's webhook URL, signed with the PSP's webhook_signing_secret.
	body := []byte(`{"event_id":"evt-` + randomHex(8) + `","event_type":"transaction.sale.success","event_body":{"merchant":{"id":"000000"},"transaction_id":"1"}}`)
	require.Equal(t, http.StatusOK, app.webhook(t, body, "your-webhook-signing-key"))
	require.Equal(t, http.StatusUnauthorized, app.webhook(t, body, "not-the-signing-key"))
}

// The browser e2e in web/e2e against this app and the platform: the React
// app built into web/dist, signing in with auth-ui and buying with
// billing-ui from the platform directly.
func TestBrowser(t *testing.T) {
	playwright, err := filepath.Abs(filepath.Join("web", "node_modules", ".bin", "playwright"))
	require.NoError(t, err)
	if _, err := os.Stat(playwright); err != nil {
		t.Skip("the app is not installed: cd web && pnpm install && pnpm build")
	}
	app := startStack(t)
	suffix := randomHex(3) // the platform keeps every run's customers: a username is unique there
	users := []string{"reader1x" + suffix, "reader2x" + suffix, "reader3x" + suffix, "reader4x" + suffix}
	for _, name := range users {
		_, err := app.auth.CreateUser(context.Background(), iam.NewUser{Email: name + "@example.com", EmailVerified: true, Username: name, Password: password})
		require.NoError(t, err)
	}
	cmd := exec.Command(playwright, "test")
	cmd.Dir = "web"
	cmd.Env = append(os.Environ(), "E2E_BASE_URL="+app.URL, "E2E_USERS="+strings.Join(users, ","), "E2E_PASSWORD="+password)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	require.NoError(t, cmd.Run())
}

var testMediaKey = mediaKey("test media key")

const (
	password     = "correct horse battery staple"
	clientSecret = "test-backend-client-secret-0123456789abcdef"
)

type stack struct {
	URL       string // the app: AuthKit's issuer, as the platform registered it
	ServerURL string // the merchant's API host
	resource  string // the platform's resource identifier
	inbox     string // Mailpit's API: the app's and the platform's mail
	auth      *authkit.Client
	bill      *openrails.Client
}

// startStack runs this app against a platform it is connected to (README.md):
// OPENRAILS_HOSTED_API_HOST, _RESOURCE and _SERVICE_TOKEN as main.go reads
// them; _APP_URL, the issuer the platform registered, whose signing key is
// in _KEYS; _MAILPIT, the API of the inbox the app's and platform's mail
// reach; and the app's own fresh database in OPENRAILS_E2E_DSN's server.
func startStack(t *testing.T) *stack {
	t.Helper()
	env := func(name string) string { return os.Getenv("OPENRAILS_HOSTED_" + name) }
	dsn := os.Getenv("OPENRAILS_E2E_DSN")
	if dsn == "" || env("API_HOST") == "" {
		t.Skip("OPENRAILS_E2E_DSN and OPENRAILS_HOSTED_* name no database and connected platform")
	}
	ctx := context.Background()
	appURL, err := url.Parse(env("APP_URL"))
	require.NoError(t, err)
	inbox, err := url.Parse(env("MAILPIT"))
	require.NoError(t, err)
	smtpPort, err := strconv.Atoi(cmpOr(env("SMTP_PORT"), "1025"))
	require.NoError(t, err)
	s := settings{
		PublicURL: env("APP_URL"), APIHost: env("API_HOST"), Merchant: cmpOr(env("MERCHANT"), "onlydemo"),
		Resource: env("RESOURCE"), ServiceToken: env("SERVICE_TOKEN"), SCIMInterval: time.Second, KeysPath: env("KEYS"),
		SMTP: smtp.Server{Host: inbox.Hostname(), Port: smtpPort, From: "OnlyDemo <hello@onlydemo.example>"},
	}
	db, err := pgxpool.New(ctx, freshDatabase(t, dsn))
	require.NoError(t, err)
	t.Cleanup(db.Close)
	ak, bill, r, err := newApp(ctx, db, s, testMediaKey)
	require.NoError(t, err)
	t.Cleanup(func() {
		bill.Close(context.Background())
		_ = ak.Close(context.Background())
	})
	listener, err := net.Listen("tcp", appURL.Host)
	require.NoError(t, err)
	app := &httptest.Server{Listener: listener, Config: &http.Server{Handler: r}}
	app.Start()
	t.Cleanup(app.Close)
	return &stack{URL: s.PublicURL, ServerURL: s.APIHost, resource: s.Resource, inbox: inbox.String(), auth: ak, bill: bill}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// freshDatabase creates a database for this test on dsn's server, dropped
// afterwards, and returns its DSN.
func freshDatabase(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "hosted_example_" + hex.EncodeToString(suffix)
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Path = "/" + name
	return u.String()
}

// customer is a signed-in user as billing-ui is: their AuthKit session,
// and the openrails:self token it trades for (as courses-web), bound to a
// DPoP key, for calling the server.
type customer struct {
	id, session string
	key         *authtest.DPoPKey
	token       string
	nonces      map[string]string // the last DPoP-Nonce each origin sent
}

// signUp creates a user with AuthKit's Go API (its sign-up route allows one
// per address a minute) and signs them in through its route.
func (a *stack) signUp(t *testing.T, name string) *customer {
	t.Helper()
	user, err := a.auth.CreateUser(context.Background(), iam.NewUser{Email: name + "@example.com", EmailVerified: true, Username: name, Password: password})
	require.NoError(t, err)
	var out struct {
		TokenSet struct {
			AccessToken string `json:"access_token"`
		} `json:"token_set"`
	}
	a.call(t, "", http.MethodPost, a.URL+"/api/v1/password/login", map[string]string{"identifier": name, "password": password}, &out)
	require.NotEmpty(t, out.TokenSet.AccessToken)
	return a.customer(t, user.ID, out.TokenSet.AccessToken)
}

// customer trades session at the app's token endpoint for a DPoP-bound
// openrails:self token, as auth-ui's resourceFetch does.
func (a *stack) customer(t *testing.T, id, session string) *customer {
	t.Helper()
	c := &customer{id: id, session: session, key: authtest.NewDPoPKey(t), nonces: map[string]string{}}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "client_id": {webClient},
		"subject_token": {session}, "subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"resource": {a.resource}, "scope": {"openrails:self"},
	}
	res := c.do(t, http.MethodPost, a.URL+"/oauth2/token", "application/x-www-form-urlencoded", []byte(form.Encode()), "")
	var tokens struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	require.Equal(t, http.StatusOK, res.status, "token exchange: %s", res.body)
	require.NoError(t, json.Unmarshal(res.body, &tokens))
	require.Equal(t, "DPoP", tokens.TokenType)
	c.token = tokens.AccessToken
	return c
}

type reply struct {
	status int
	body   []byte
}

// do sends one request with a fresh DPoP proof (and token, when set),
// retrying once with the server's nonce when it asks for one.
func (c *customer) do(t *testing.T, method, target, contentType string, body []byte, token string) reply {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	origin := u.Scheme + "://" + u.Host
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(method, target, bytes.NewReader(body))
		require.NoError(t, err)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if token != "" {
			c.key.Authorize(t, req, token, c.nonces[origin])
		} else {
			htu := *u
			htu.RawQuery = ""
			req.Header.Set("DPoP", c.key.Proof(t, method, htu.String(), "", c.nonces[origin]))
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		out, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if nonce := res.Header.Get("DPoP-Nonce"); nonce != "" {
			c.nonces[origin] = nonce
		}
		asks := strings.Contains(res.Header.Get("WWW-Authenticate"), "use_dpop_nonce") || bytes.Contains(out, []byte("use_dpop_nonce"))
		if attempt == 0 && asks && res.Header.Get("DPoP-Nonce") != "" {
			continue
		}
		return reply{res.StatusCode, out}
	}
}

// buy pays for product's price as billing-ui's BuyButton does: the customer
// creates a checkout session at the server with their DPoP-bound token, and
// CheckoutModal reads and pays it by its id with a new card.
func (a *stack) buy(t *testing.T, c *customer, product, price string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"product_key": product, "price_key": price})
	res := c.do(t, http.MethodPost, a.ServerURL+"/v1/me/checkout-sessions", "application/json", payload, c.token)
	require.Less(t, res.status, 300, "checkout session: %d %s", res.status, res.body)
	var session struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(res.body, &session))
	var page struct {
		Options []struct {
			ID     string `json:"id"`
			Driver string `json:"driver"`
		} `json:"options"`
	}
	a.call(t, "", http.MethodGet, a.ServerURL+"/v1/checkout-sessions/"+session.ID, nil, &page)
	require.Len(t, page.Options, 1)
	require.Equal(t, "collect_js", page.Options[0].Driver)
	var paid struct {
		Status string `json:"status"`
	}
	a.call(t, "", http.MethodPost, a.ServerURL+"/v1/checkout-sessions/"+session.ID+"/pay", map[string]any{
		"option_id":       page.Options[0].ID,
		"payment_token":   "e2e-" + randomHex(8) + "-4242", // Collect.js's, as the platform's nmimock accepts
		"billing_details": map[string]any{"name": "Card Holder", "address": map[string]string{"country": "US", "postal_code": "10001"}},
	}, &paid)
	require.Equal(t, "succeeded", paid.Status)
}

// mail is a message in Mailpit.
type mail struct {
	ID   string
	Text string
}

// mailTo waits for an email to address that match accepts, in Mailpit.
func (a *stack) mailTo(t *testing.T, address string, match func(mail) bool) mail {
	t.Helper()
	var found mail
	require.Eventually(t, func() bool {
		var list struct{ Messages []mail }
		if err := getJSON(a.inbox+"/api/v1/search?query="+url.QueryEscape("to:"+address), &list); err != nil {
			return false
		}
		for _, m := range list.Messages {
			if err := getJSON(a.inbox+"/api/v1/message/"+m.ID, &m); err == nil && match(m) {
				found = m
				return true
			}
		}
		return false
	}, 60*time.Second, 250*time.Millisecond, "an email to %s", address)
	return found
}

func getJSON(target string, out any) error {
	res, err := http.Get(target)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(res.Body).Decode(out)
}

// nmiSignature is NMI's Webhook-Signature s: HMAC-SHA256 of "t.body".
func nmiSignature(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return hex.EncodeToString(mac.Sum(nil))
}

// webhook posts body to the NMI PSP's webhook URL signed with secret, as NMI does.
func (a *stack) webhook(t *testing.T, body []byte, secret string) int {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequest(http.MethodPost, a.ServerURL+"/v1/webhooks/nmi/000000", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Webhook-Signature", "t="+ts+",s="+nmiSignature(secret, ts, body))
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = res.Body.Close()
	return res.StatusCode
}

// requireBuy: path answers 402 naming the products that unlock it and the
// buy page.
func (a *stack) requireBuy(t *testing.T, token, path string, products []string, buy string) {
	t.Helper()
	res := a.get(t, token, path)
	require.Equal(t, http.StatusPaymentRequired, res.StatusCode, path)
	var body struct {
		Error, Buy string
		Products   []string
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Equal(t, "access_required", body.Error)
	require.Equal(t, products, body.Products)
	require.Equal(t, buy, body.Buy)
}

// requireVideo: path answers a signed URL that serves file, ranges included.
func (a *stack) requireVideo(t *testing.T, token, path, file string) {
	t.Helper()
	res := a.get(t, token, path)
	require.Equal(t, http.StatusOK, res.StatusCode, path)
	var body struct {
		VideoURL string `json:"video_url"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	want, err := os.ReadFile(file)
	require.NoError(t, err)

	video := a.get(t, "", body.VideoURL) // the URL is the credential
	require.Equal(t, http.StatusOK, video.StatusCode)
	require.Equal(t, "video/mp4", video.Header.Get("Content-Type"))
	got, err := io.ReadAll(video.Body)
	require.NoError(t, err)
	require.Equal(t, want, got)

	req, err := http.NewRequest(http.MethodGet, a.URL+body.VideoURL, nil)
	require.NoError(t, err)
	req.Header.Set("Range", "bytes=0-99")
	ranged, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = ranged.Body.Close() }()
	require.Equal(t, http.StatusPartialContent, ranged.StatusCode)
	part, err := io.ReadAll(ranged.Body)
	require.NoError(t, err)
	require.Equal(t, want[:100], part)
}

// get requests the app's path without following redirects.
func (a *stack) get(t *testing.T, token, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, a.URL+path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// call sends a JSON request to target, from the app's origin, and decodes a
// 2xx answer into out.
func (a *stack) call(t *testing.T, token, method, target string, body, out any) {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&payload).Encode(body))
	}
	req, err := http.NewRequest(method, target, &payload)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", a.URL)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	var raw json.RawMessage
	_ = json.NewDecoder(res.Body).Decode(&raw)
	require.Less(t, res.StatusCode, 300, "%s %s: %d %s", method, target, res.StatusCode, raw)
	if out != nil && len(raw) > 0 {
		require.NoError(t, json.Unmarshal(raw, out))
	}
}
