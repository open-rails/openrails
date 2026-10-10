// Command standalone is the embedded example's course site with OpenRails run
// as its own server (openrails/ holds the server's files). The app keeps its
// AuthKit, which is now also the OAuth issuer the server trusts: it mints the
// browser's tokens for /v1/me, the backend's client-credentials
// tokens, and pushes its users to the server's AuthKit over SCIM. newBilling builds the
// client with NewRemote; content.go and web/src/pages.tsx are the embedded
// example's, unchanged.
//
// Run it from this directory with DATABASE_URL, after building the app
// (cd web && pnpm install && pnpm build) and starting the server (README.md).
// PUBLIC_URL is this app's origin (default http://localhost:8080), which is
// AuthKit's issuer; OPENRAILS_URL is the server's (default
// http://localhost:3053), which is also its resource identifier;
// OPENRAILS_CLIENT_SECRET is the backend client's secret (32 bytes or more).
// ADDR is the listen address (default :8080); EXAMPLE_CHECK_ONLY=1 boots,
// checks the server and exits.
package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"net/url"
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
	"golang.org/x/oauth2/clientcredentials"

	"github.com/open-rails/openrails"
)

// settings are the app's deployment: where it and the OpenRails server are,
// the secret its backend authenticates with, and its mail server.
type settings struct {
	PublicURL    string        // this app: AuthKit's issuer and the browser's origin
	OpenRailsURL string        // the server, and its resource identifier (auth.resource.id)
	Merchant     string        // the merchant the server serves for this app (its merchant.yaml)
	ClientSecret string        // the backend client's secret
	SCIMInterval time.Duration // how often AuthKit pushes user changes to the server
	SMTP         smtp.Server   // AuthKit's mail: the codes that prove each user's email
}

// The OAuth clients this app registers in its AuthKit for OpenRails.
const (
	webClient     = "courses-web"     // the React app: trades the user's session for an OpenRails token
	backendClient = "courses-backend" // this server: its own client-credentials tokens
)

// newAuth is a development AuthKit, now also the authorization server
// OpenRails trusts: it mints access tokens for the OpenRails server and
// pushes this app's users to it over SCIM. Users prove their email: OpenRails
// is told only a proven address, and sends receipts to it.
func newAuth(ctx context.Context, db *pgxpool.Pool, s settings) (*authkit.Client, error) {
	email, err := smtp.New(smtp.Config{Server: s.SMTP, AppName: "OnlyDemo"})
	if err != nil {
		return nil, err
	}
	secret := sha256.Sum256([]byte(s.ClientSecret))
	return authkit.New(ctx, authkit.Config{
		Database:     authkit.DatabaseConfig{Schema: "profiles"},
		Token:        authkit.TokenConfig{Issuer: s.PublicURL, IssuedAudiences: []string{"onlydemo"}},
		Keys:         authkit.KeysConfig{AllowEphemeralDevKeys: true}, // the README's Path: "/vault/auth" in production
		HTTP:         &authkit.HTTPConfig{DirectPeerIP: true, RefreshCookie: true},
		Registration: authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen, Verification: iam.RegistrationVerificationRequired},
		TwoFactor:    authkit.TwoFactorConfig{Mode: iam.TwoFactorDisabled},

		// OpenRails is a resource server this AuthKit mints tokens for.
		AuthorizationServer: authkit.AuthorizationServerConfig{
			Resources: []authkit.ResourceServerConfig{{
				ID: s.OpenRailsURL, // the tokens' aud
				// A customer's own billing; the merchant API.
				Scopes: []string{"openrails:self", "openrails:merchant"},
				// The most a token may carry.
				Permissions: []string{"merchant:billing:read", "merchant:entitlements:read", "merchant:catalog:read", "merchant:directory:manage"},
				// The user's email in every token: a receipt reaches a brand-new buyer.
				ContactClaims: true,
			}},
			Clients: []authkit.OAuthClientConfig{{
				ID:         webClient, // public: the browser proves a DPoP key instead of a secret
				Origins:    []string{s.PublicURL},
				Resources:  []string{s.OpenRailsURL},
				GrantTypes: []authkit.OAuthGrantType{authkit.GrantTokenExchange},
			}, {
				ID:           backendClient, // confidential: this server and the SCIM pushes
				SecretSHA256: hex.EncodeToString(secret[:]),
				Resources:    []string{s.OpenRailsURL},
				// Customer support's reads, the content gate, offers, and the SCIM pushes.
				Permissions: []string{"merchant:billing:read", "merchant:entitlements:read", "merchant:catalog:read", "merchant:directory:manage"},
				GrantTypes:  []authkit.OAuthGrantType{authkit.GrantClientCredentials},
			}},
		},

		// Customer contacts: every user, pushed to the merchant's directory in
		// the server's AuthKit with the backend client's tokens, so receipts
		// and the admin customer read have each email.
		Provisioning: authkit.ProvisioningConfig{
			Interval: s.SCIMInterval,
			Targets: []authkit.ProvisioningTarget{{
				Name:              "openrails",
				URL:               s.OpenRailsURL + "/directory/scim/v2",
				ClientCredentials: backendCredentials(s),
			}},
		},
	}, authkit.Deps{Postgres: db, Email: email})
}

// backendCredentials are the backend client's: client credentials at this
// app's own token endpoint, for the OpenRails resource.
func backendCredentials(s settings) *authkit.ProvisioningClientCredentials {
	return &authkit.ProvisioningClientCredentials{
		TokenURL:     s.PublicURL + "/oauth2/token",
		ClientID:     backendClient,
		ClientSecret: s.ClientSecret,
		Scopes:       []string{"openrails:merchant"},
		Resource:     s.OpenRailsURL,
	}
}

// newBilling is the remote client: the same *openrails.Client as embedded,
// over HTTP, with a client-credentials token from this app's AuthKit.
func newBilling(ctx context.Context, s settings) (*openrails.Client, error) {
	cc := backendCredentials(s)
	tokens := (&clientcredentials.Config{
		ClientID: cc.ClientID, ClientSecret: cc.ClientSecret, TokenURL: cc.TokenURL, Scopes: cc.Scopes,
		EndpointParams: url.Values{"resource": {cc.Resource}},
	}).TokenSource(context.WithoutCancel(ctx)) // caches each token until it expires
	return openrails.NewRemote(s.OpenRailsURL,
		openrails.WithTokenProvider(func(context.Context) (string, error) {
			t, err := tokens.Token()
			if err != nil {
				return "", err
			}
			return t.AccessToken, nil
		}),
		openrails.WithDefaultMerchant(s.Merchant), // the issuer serves one merchant; naming it is explicit
		openrails.WithTimeout(5*time.Second),
	)
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
		c.JSON(http.StatusOK, gin.H{"base_url": s.OpenRailsURL + "/v1", "resource": s.OpenRailsURL})
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
		OpenRailsURL: cmp.Or(os.Getenv("OPENRAILS_URL"), "http://localhost:3053"),
		Merchant:     cmp.Or(os.Getenv("OPENRAILS_MERCHANT"), "onlydemo"),
		ClientSecret: os.Getenv("OPENRAILS_CLIENT_SECRET"),
		SCIMInterval: 30 * time.Second,
		SMTP: smtp.Server{
			Host: cmp.Or(os.Getenv("EMAIL_SMTP_HOST"), "localhost"),
			Port: cmp.Or(atoi(os.Getenv("EMAIL_SMTP_PORT")), 1025),
			From: "OnlyDemo <hello@onlydemo.example>",
		},
	}
	if len(s.ClientSecret) < 32 {
		return errors.New("OPENRAILS_CLIENT_SECRET must be at least 32 bytes")
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
