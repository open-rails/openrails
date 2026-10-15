// Command hosted is the embedded example's course site on the hosted
// platform: OpenRails runs there, for many merchants, and this app is one
// merchant's. Its backend calls the platform with the service token the
// console issued (`go run . catalog apply catalog.yaml` applies the catalog
// with it; starting the app never does); its AuthKit mints the browser's
// DPoP-bound tokens for /v1/me and pushes its users over SCIM. content.go and
// web/src/pages.tsx are the embedded example's, unchanged.
//
// Run it from this directory with DATABASE_URL, after building the app
// (cd web && pnpm install && pnpm build) and connecting it (README.md):
// OPENRAILS_URL is the platform's API, OPENRAILS_RESOURCE its resource
// identifier, OPENRAILS_SERVICE_TOKEN the service token.
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
)

// settings are the app's deployment: where it is, its merchant on the
// platform, and its mail server.
type settings struct {
	PublicURL    string        // this app: AuthKit's issuer and the browser's origin
	OpenRailsURL string        // the platform's API
	Resource     string        // the platform's resource identifier: its tokens' aud
	ServiceToken string        // the console's service token for this app, bound to its registration
	Directory    string        // the platform's directory for this app's users (its AuthKit's /directory/scim/v2)
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
	// A deployment keeps this directory ID across every restart.
	tenant, err := iam.ParseTenantID(cmp.Or(os.Getenv("AUTH_DEFAULT_TENANT_ID"), "8c67667c-8837-439a-a485-2c9a385c64c3"))
	if err != nil {
		return nil, err
	}
	return authkit.New(ctx, authkit.Config{
		DefaultTenantID: tenant,
		Database:        authkit.DatabaseConfig{Schema: "profiles"},
		Token:           authkit.TokenConfig{Issuer: s.PublicURL, IssuedAudiences: []string{"onlydemo"}},
		Keys:            authkit.KeysConfig{Path: s.KeysPath, AllowEphemeralDevKeys: true}, // generated once, then reused
		HTTP:            &authkit.HTTPConfig{DirectPeerIP: true, RefreshCookie: true},
		Registration:    authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen, Verification: iam.RegistrationVerificationRequired},
		TwoFactor:       authkit.TwoFactorConfig{Mode: iam.TwoFactorDisabled},

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

		// Customer contacts: every user, pushed to the merchant's directory on
		// the platform with the service token.
		Provisioning: authkit.ProvisioningConfig{
			Interval: s.SCIMInterval,
			Targets: []authkit.ProvisioningTarget{{
				Name:        "openrails",
				URL:         s.Directory,
				BearerToken: s.ServiceToken,
			}},
		},
	}, authkit.Deps{Postgres: db, Email: email})
}

// newBilling is the remote handle: the same *openrails.Client as embedded,
// over HTTP to the platform, with the service token, which is the
// merchant's: every call acts on that merchant.
func newBilling(s settings) (*openrails.Client, error) {
	return openrails.NewRemoteMerchant(s.OpenRailsURL, openrails.RemoteCredential{APIKey: s.ServiceToken})
}

// newApp builds the app: its AuthKit (started: its River workers push the
// SCIM changes), the billing client and the router.
func newApp(ctx context.Context, db *pgxpool.Pool, s settings, media mediaKey) (*authkit.Client, *openrails.Client, *gin.Engine, error) {
	ak, err := newAuth(ctx, db, s) // see AuthKit's README
	if err != nil {
		return nil, nil, nil, err
	}
	bill, err := newBilling(s)
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
		c.JSON(http.StatusOK, gin.H{"base_url": s.OpenRailsURL + "/v1", "resource": s.Resource})
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
		OpenRailsURL: os.Getenv("OPENRAILS_URL"),
		Resource:     os.Getenv("OPENRAILS_RESOURCE"),
		ServiceToken: os.Getenv("OPENRAILS_SERVICE_TOKEN"),
		Directory:    os.Getenv("OPENRAILS_DIRECTORY"),
		SCIMInterval: 30 * time.Second,
		KeysPath:     cmp.Or(os.Getenv("AUTH_KEYS_PATH"), ".dev/auth"),
		SMTP: smtp.Server{
			Host: cmp.Or(os.Getenv("EMAIL_SMTP_HOST"), "localhost"),
			Port: cmp.Or(atoi(os.Getenv("EMAIL_SMTP_PORT")), 1025),
			From: "OnlyDemo <hello@onlydemo.example>",
		},
	}
	if s.OpenRailsURL == "" || s.Resource == "" || s.ServiceToken == "" || s.Directory == "" {
		return errors.New("OPENRAILS_URL, OPENRAILS_RESOURCE, OPENRAILS_SERVICE_TOKEN and OPENRAILS_DIRECTORY are required: see README.md")
	}
	if len(os.Args) == 4 && os.Args[1] == "catalog" && os.Args[2] == "apply" {
		bill, err := newBilling(s)
		if err != nil {
			return err
		}
		return applyCatalog(ctx, bill, os.Args[3], os.Stdout)
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

	if os.Getenv("EXAMPLE_CHECK_ONLY") != "" {
		_, err := bill.ListMeters(ctx, billing.PageRequest{Limit: 1}) // reaches the platform as the app's merchant
		return err
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
