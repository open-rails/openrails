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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/adapters/smtp"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/helpers/smtp/smtptest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest/nmimock"
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

// The server's setup, exercised: a user who signs up and proves their email
// buys and is emailed a receipt, the backend's read-only token reads them
// with the email AuthKit pushed and is refused a write, and the PSP's signed
// webhooks are accepted while a forged one is refused.
func TestServerSetup(t *testing.T) {
	app := startStack(t)
	ctx := context.Background()

	// Sign up as the store's sign-in dialog does: register, then the code
	// AuthKit emailed proves the address.
	app.call(t, "", http.MethodPost, app.URL+"/api/v1/register", map[string]string{"identifier": "erin@example.com", "username": "erin", "password": password}, nil)
	sixDigits := regexp.MustCompile(`\b\d{6}\b`)
	code := sixDigits.FindString(app.mailTo(t, "erin@example.com", func(m smtptest.Message) bool { return sixDigits.MatchString(m.Text) }).Text)
	var signedIn struct {
		TokenSet struct {
			AccessToken string `json:"access_token"`
		} `json:"token_set"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	app.call(t, "", http.MethodPost, app.URL+"/api/v1/verify/confirm", map[string]string{"identifier": "erin@example.com", "code": code}, &signedIn)
	require.NotEmpty(t, signedIn.TokenSet.AccessToken)
	erin, err := billing.ParseCustomerID(signedIn.User.ID)
	require.NoError(t, err)

	// Her purchase makes her a customer, receipted to that address.
	app.buy(t, app.customer(t, signedIn.User.ID, signedIn.TokenSet.AccessToken), "course-101", "purchase")
	app.mailTo(t, "erin@example.com", func(m smtptest.Message) bool { return strings.Contains(m.Text, "4.99 USD") })

	// AuthKit's SCIM push keeps her contact in the merchant's directory at
	// the server: the admin customer read shows it.
	require.Eventually(t, func() bool {
		c, err := app.bill.GetCustomer(ctx, erin)
		return err == nil && c.Contact != nil && c.Contact.Email != nil && *c.Contact.Email == "erin@example.com"
	}, 30*time.Second, 250*time.Millisecond, "the admin customer read shows her email")

	// The backend's token carries merchant:billing:read: support's writes
	// (merchant:billing:manage) are refused.
	_, err = app.bill.UpdateCustomer(ctx, erin, billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 1_000_000}}})
	require.ErrorIs(t, err, billing.ErrDenied)

	// NMI posts to /v1/webhooks/nmi/{gateway id}, signed with the PSP's webhook_signing_secret.
	body := []byte(`{"event_id":"evt-1","event_type":"transaction.sale.success","event_body":{"merchant":{"id":"000000"},"transaction_id":"1"}}`)
	require.Equal(t, http.StatusOK, app.webhook(t, body, "your-webhook-signing-key"))
	require.Equal(t, http.StatusUnauthorized, app.webhook(t, body, "not-the-signing-key"))
}

// The browser e2e in web/e2e against this app and the server: the React app
// built into web/dist, signing in with auth-ui and buying with billing-ui
// from the server directly.
func TestBrowser(t *testing.T) {
	playwright, err := filepath.Abs(filepath.Join("web", "node_modules", ".bin", "playwright"))
	require.NoError(t, err)
	if _, err := os.Stat(playwright); err != nil {
		t.Skip("the app is not installed: cd web && pnpm install && pnpm build")
	}
	app := startStack(t)
	users := []string{"reader1", "reader2", "reader3", "reader4"}
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
	URL       string // the app, http://localhost:port: AuthKit's issuer
	ServerURL string // the OpenRails server
	auth      *authkit.Client
	bill      *openrails.Client
	nmi       *nmimock.Mock
	mail      *smtptest.Server
}

// startStack runs what compose.yaml runs, on fresh databases in
// OPENRAILS_E2E_DSN's server, with OPENRAILS_E2E_REDIS's Redis when it names
// one (else without Redis): the openrails binary
// with openrails/config.yaml and merchant.example.yaml (only its ports
// changed), the catalog applied with apply-catalog, NMI played by nmimock,
// one SMTP server for both, and this app. OPENRAILS_BIN names the binary;
// without it the server module is built.
func startStack(t *testing.T) *stack {
	t.Helper()
	dsn, redisAddr := os.Getenv("OPENRAILS_E2E_DSN"), os.Getenv("OPENRAILS_E2E_REDIS")
	if dsn == "" {
		t.Skip("OPENRAILS_E2E_DSN names no PostgreSQL 18 server")
	}
	ctx := context.Background()
	gateway := nmimock.New(nmimock.Options{})
	t.Cleanup(gateway.Close)
	mail := smtptest.Start(t, smtptest.Options{})

	appListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	appURL := "http://localhost:" + strconv.Itoa(appListener.Addr().(*net.TCPAddr).Port)
	serverPort := freePort(t)
	serverURL := "http://localhost:" + strconv.Itoa(serverPort)

	// The server, as compose.yaml runs it.
	dir := t.TempDir()
	config, err := os.ReadFile("openrails/config.yaml")
	require.NoError(t, err)
	config = bytes.ReplaceAll(config, []byte("localhost:8080"), []byte(strings.TrimPrefix(appURL, "http://")))
	config = bytes.ReplaceAll(config, []byte("localhost:3053"), []byte(strings.TrimPrefix(serverURL, "http://")))
	if redisAddr == "" { // Redis is optional: one server without it counts its limits in memory
		config = regexp.MustCompile(`(?m)^redis:.*\n(?:  .*\n)*`).ReplaceAll(config, nil)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), config, 0o600))
	manifest, err := os.ReadFile("openrails/merchant.example.yaml")
	require.NoError(t, err)
	manifest = bytes.ReplaceAll(manifest, []byte("localhost:8080"), []byte(strings.TrimPrefix(appURL, "http://")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "merchant.yaml"), manifest, 0o600))
	catalogFile, err := filepath.Abs("catalog.yaml")
	require.NoError(t, err)
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir,
		"DB_URL=" + freshDatabase(t, dsn),
		"HOST=127.0.0.1", "PORT=" + strconv.Itoa(serverPort), "AUTH_KEYS_PATH=" + filepath.Join(dir, "keys"),
		"EMAIL_SMTP_HOST=" + mail.Host, "EMAIL_SMTP_PORT=" + strconv.Itoa(mail.Port),
		"PROVIDER_SANDBOX_NMI_GATEWAY_URL=" + gateway.URL(),
		"RATE_LIMITS_DISABLED=true", // every request here comes from 127.0.0.1
	}
	if redisAddr != "" {
		env = append(env, "REDIS_ADDR="+redisAddr)
	}
	bin := openrailsBinary(t)
	server := exec.Command(bin, "run-server", "--config", "config.yaml", "--merchant-manifest", "merchant.yaml")
	server.Dir, server.Env = dir, env
	logFile, err := os.Create(filepath.Join(dir, "server.log"))
	require.NoError(t, err)
	server.Stdout, server.Stderr = logFile, logFile
	require.NoError(t, server.Start())
	t.Cleanup(func() {
		_ = server.Process.Signal(os.Interrupt)
		_ = server.Wait()
		if t.Failed() {
			out, _ := os.ReadFile(logFile.Name())
			t.Logf("openrails server log:\n%s", tail(out, 80))
		}
	})
	require.Eventually(t, func() bool {
		res, err := http.Get(serverURL + "/health/ready")
		if err != nil {
			return false
		}
		_ = res.Body.Close()
		return res.StatusCode == http.StatusOK
	}, 60*time.Second, 200*time.Millisecond, "the server is ready")
	apply := exec.Command(bin, "apply-catalog", "--config", "config.yaml", "--merchant-manifest", "merchant.yaml", "--merchant", "onlydemo", "--file", catalogFile)
	apply.Dir, apply.Env = dir, env
	out, err := apply.CombinedOutput()
	require.NoError(t, err, "apply-catalog: %s", tail(out, 20))

	// The app.
	s := settings{
		PublicURL: appURL, OpenRailsURL: serverURL, Merchant: "onlydemo", ClientSecret: clientSecret,
		SCIMInterval: time.Second, SMTP: smtp.Server{Host: mail.Host, Port: mail.Port, From: "OnlyDemo <hello@onlydemo.example>"},
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
	app := &httptest.Server{Listener: appListener, Config: &http.Server{Handler: r}}
	app.Start()
	t.Cleanup(app.Close)
	return &stack{URL: appURL, ServerURL: serverURL, auth: ak, bill: bill, nmi: gateway, mail: mail}
}

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// openrailsBinary is OPENRAILS_BIN, or the server module built once.
func openrailsBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("OPENRAILS_BIN"); bin != "" {
		return bin
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "openrails-bin")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "openrails")
		build := exec.Command("go", "build", "-o", builtBin, "./cmd/openrails")
		build.Dir = filepath.Join("..", "..", "server")
		build.Env = append(os.Environ(), "GOWORK=off")
		if out, err := build.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./server/cmd/openrails: %v\n%s", err, out)
		}
	})
	require.NoError(t, buildErr)
	return builtBin
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func tail(b []byte, lines int) string {
	all := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return strings.Join(all[max(0, len(all)-lines):], "\n")
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
	name := "standalone_example_" + hex.EncodeToString(suffix)
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
		"resource": {a.ServerURL}, "scope": {"openrails:self"},
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
		"payment_token":   a.nmi.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}),
		"billing_details": map[string]any{"name": "Card Holder", "address": map[string]string{"country": "US", "postal_code": "10001"}},
	}, &paid)
	require.Equal(t, "succeeded", paid.Status)
}

// mailTo waits for an email to address that match accepts.
func (a *stack) mailTo(t *testing.T, address string, match func(smtptest.Message) bool) smtptest.Message {
	t.Helper()
	var found smtptest.Message
	require.Eventually(t, func() bool {
		for _, m := range a.mail.Messages() {
			if slices.Equal(m.To, []string{address}) && match(m) {
				found = m
				return true
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond, "an email to %s", address)
	return found
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
