// Command hosted is the embedded example's course site on the hosted
// platform: OpenRails runs there, for many merchants, and this app is one
// merchant's. Its backend calls the merchant's API host with the service
// token the console issued, and applies catalog.yaml with it at boot; its
// AuthKit mints the browser's DPoP-bound tokens for /v1/me and pushes its
// users over SCIM. content.go and web/src/pages.tsx are the embedded
// example's, unchanged.
//
// Run it from this directory with DATABASE_URL, after building the app
// (cd web && pnpm install && pnpm build) and connecting it (README.md):
// OPENRAILS_API_HOST is the merchant's API host, OPENRAILS_RESOURCE the
// platform's resource identifier, OPENRAILS_SERVICE_TOKEN the service token.
// PUBLIC_URL is this app's origin (default http://localhost:8080), which is
// AuthKit's issuer. ADDR is the listen address (default :8080);
// EXAMPLE_CHECK_ONLY=1 boots, checks the platform and exits.
package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	authkitgin "github.com/open-rails/authkit/adapters/gin"
	"github.com/open-rails/authkit/adapters/smtp"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// settings are the app's deployment: where it is, its merchant on the
// platform, and its mail server.
type settings struct {
	PublicURL    string        // this app: AuthKit's issuer and the browser's origin
	APIHost      string        // the merchant's API host on the platform
	Merchant     string        // the merchant's name on the platform
	Resource     string        // the platform's resource identifier: its tokens' aud
	ServiceToken string        // the console's service token for this app
	SCIMInterval time.Duration // how often AuthKit pushes user changes to the platform
	SMTP         smtp.Server   // AuthKit's mail: the codes that prove each user's email
	KeysPath     string        // AuthKit's signing key, kept across restarts: the platform trusts it
}

// webClient is the OAuth client the React app trades the user's session
// through for a platform token.
const webClient = "courses-web"

// newAuth is a development AuthKit, also the issuer the platform trusts for
// this merchant's customers: it mints their tokens for the platform and
// pushes this app's users to it over SCIM. Users prove their email: the
// platform is told only a proven address, and sends receipts to it.
func newAuth(ctx context.Context, db *pgxpool.Pool, s settings) (*authkit.Client, error) {
	email, err := smtp.New(smtp.Config{Server: s.SMTP, AppName: "OnlyDemo"})
	if err != nil {
		return nil, err
	}
	return authkit.New(ctx, authkit.Config{
		Database:     authkit.DatabaseConfig{Schema: "profiles"},
		Token:        authkit.TokenConfig{Issuer: s.PublicURL, IssuedAudiences: []string{"onlydemo"}},
		Keys:         authkit.KeysConfig{Path: s.KeysPath, AllowEphemeralDevKeys: true}, // generated once, then reused
		HTTP:         &authkit.HTTPConfig{DirectPeerIP: true, RefreshCookie: true},
		Registration: authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen, Verification: iam.RegistrationVerificationRequired},
		TwoFactor:    authkit.TwoFactorConfig{Mode: iam.TwoFactorDisabled},

		// The platform is a resource server this AuthKit mints customer tokens for.
		AuthorizationServer: authkit.AuthorizationServerConfig{
			Resources: []authkit.ResourceServerConfig{{
				ID:            s.Resource,                 // the tokens' aud
				Scopes:        []string{"openrails:self"}, // a customer's own billing
				ContactClaims: true,                       // the user's email in every token: a receipt reaches a brand-new buyer
			}},
			Clients: []authkit.OAuthClientConfig{{
				ID:         webClient, // public: the browser proves a DPoP key instead of a secret
				Origins:    []string{s.PublicURL},
				Resources:  []string{s.Resource},
				GrantTypes: []authkit.OAuthGrantType{authkit.GrantTokenExchange},
			}},
		},

		// Customer contacts: every user, pushed to the merchant's SCIM routes
		// with the service token.
		Provisioning: authkit.ProvisioningConfig{
			Interval: s.SCIMInterval,
			Targets: []authkit.ProvisioningTarget{{
				Name:        "openrails",
				URL:         s.APIHost + "/v1/app/scim/v2",
				BearerToken: s.ServiceToken,
			}},
		},
	}, authkit.Deps{Postgres: db, Email: email})
}

// newBilling is the remote client: the same *openrails.Client as embedded,
// over HTTP to the merchant's API host, with the service token. Like
// embedded, catalog.yaml is the truth: each distinct document applies once.
func newBilling(ctx context.Context, s settings) (*openrails.Client, error) {
	bill, err := openrails.NewRemote(s.APIHost,
		openrails.WithAPIKey(s.ServiceToken),
		openrails.WithDefaultMerchant(s.Merchant), // the token is the merchant's, but each call still names it
		openrails.WithTimeout(5*time.Second),
	)
	if err != nil {
		return nil, err
	}
	products, err := catalog.ReadFile("catalog.yaml")
	if err != nil {
		return nil, err
	}
	if _, err := bill.ApplyCatalog(ctx, products, billing.ApplyCatalogParams{}); err != nil { // a field the console edited since is skipped, not overwritten
		return nil, err
	}
	return bill, nil
}

// newApp builds the app: its AuthKit (started: its River workers push the
// SCIM changes), the billing client and the router.
func newApp(ctx context.Context, db *pgxpool.Pool, s settings, media mediaKey) (*authkit.Client, *openrails.Client, *gin.Engine, error) {
	ak, err := newAuth(ctx, db, s) // see AuthKit's README
	if err != nil {
		return nil, nil, nil, err
	}
	bill, err := newBilling(ctx, s)
	if err == nil {
		err = ak.Start(ctx)
	}
	if err != nil {
		_ = ak.Close(context.WithoutCancel(ctx))
		return nil, nil, nil, err
	}

	r := gin.Default()
	if err := authkitgin.Mount(r, ak); err != nil { // sign-up and sign-in under /api/v1, the token endpoint at /oauth2/token
		return nil, nil, nil, err
	}
	r.GET("/api/billing", func(c *gin.Context) { // where the browser's billing client calls
		c.JSON(http.StatusOK, gin.H{"base_url": s.APIHost + "/v1", "resource": s.Resource})
	})
	courseRoutes(r, ak, bill, media) // the course list, the gate on each course and its media
	serveApp(r, "web/dist")          // the React app: its pages, the buy page among them
	return ak, bill, r, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	s := settings{
		PublicURL:    cmp.Or(os.Getenv("PUBLIC_URL"), "http://localhost:8080"),
		APIHost:      os.Getenv("OPENRAILS_API_HOST"),
		Merchant:     cmp.Or(os.Getenv("OPENRAILS_MERCHANT"), "onlydemo"),
		Resource:     os.Getenv("OPENRAILS_RESOURCE"),
		ServiceToken: os.Getenv("OPENRAILS_SERVICE_TOKEN"),
		SCIMInterval: 30 * time.Second,
		KeysPath:     cmp.Or(os.Getenv("AUTH_KEYS_PATH"), ".dev/auth"),
		SMTP: smtp.Server{
			Host: cmp.Or(os.Getenv("EMAIL_SMTP_HOST"), "localhost"),
			Port: cmp.Or(atoi(os.Getenv("EMAIL_SMTP_PORT")), 1025),
			From: "OnlyDemo <hello@onlydemo.example>",
		},
	}
	if s.APIHost == "" || s.Resource == "" || s.ServiceToken == "" {
		return errors.New("OPENRAILS_API_HOST, OPENRAILS_RESOURCE and OPENRAILS_SERVICE_TOKEN are required: see README.md")
	}
	db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()

	media := mediaKey(os.Getenv("MEDIA_KEY"))
	if len(media) == 0 {
		media = make(mediaKey, 32)
		_, _ = rand.Read(media)
	}
	ak, bill, r, err := newApp(ctx, db, s, media)
	if err != nil {
		return err
	}
	defer func() { _ = ak.Close(context.WithoutCancel(ctx)) }()
	defer bill.Close(context.WithoutCancel(ctx))

	if os.Getenv("EXAMPLE_CHECK_ONLY") != "" {
		return bill.Ready(ctx)
	}
	addr := cmp.Or(os.Getenv("ADDR"), ":8080")
	server := &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.WithoutCancel(ctx))
	}()
	return server.ListenAndServe()
}

// atoi is s as a number, 0 when it is not one.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
