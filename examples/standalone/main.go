// Command standalone is the embedded example's course site with OpenRails run
// as its own server (openrails/ holds the server's files). The app keeps its
// AuthKit, which is now also the OAuth issuer the server trusts: it mints the
// browser's tokens for /v1/me, carrying the user's proven email, and the
// backend's client-credentials tokens. newBilling builds the client with
// NewRemote; content.go and web/src/pages.tsx are the embedded example's,
// unchanged.
//
// Run it from this directory with DATABASE_URL, after building the app
// (cd web && pnpm install && pnpm build) and starting the server (README.md).
// PUBLIC_URL is this app's origin (default http://localhost:8080), which is
// AuthKit's issuer; OPENRAILS_URL is the server's (default
// http://localhost:3053), which is also its resource identifier;
// OPENRAILS_MERCHANT_ID is the merchant's ID on it;
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
	"github.com/open-rails/openrails/billing"
)

// settings are the app's deployment: where it and the OpenRails server are,
// the secret its backend authenticates with, and its mail server.
type settings struct {
	PublicURL    string             // this app: AuthKit's issuer and the browser's origin
	OpenRailsURL string             // the server, and its resource identifier (public_url)
	Merchant     billing.MerchantID // the merchant the server trusts this app's issuer for
	ClientSecret string             // the backend client's secret
	SMTP         smtp.Server        // AuthKit's mail: the codes that prove each user's email
}

// The OAuth clients this app registers in its AuthKit for OpenRails.
const (
	webClient     = "courses-web"     // the React app: trades the user's session for an OpenRails token
	backendClient = "courses-backend" // this server: its own client-credentials tokens
)

// backendPermissions are what the backend's tokens carry on the server:
// customer support's reads, the content gate and catalog, by OpenRails' names
// for the merchant's own staff.
var backendPermissions = []string{
	string(billing.MerchantCustomerRead), string(billing.MerchantEntitlementsRead), string(billing.MerchantCatalogRead),
}

// newAuth is a development AuthKit, now also the authorization server
// OpenRails trusts: it mints access tokens for the OpenRails server. Users
// prove their email, and a token asked for with the email scope carries it:
// OpenRails is told only a proven address, and sends receipts to it.
func newAuth(ctx context.Context, db *pgxpool.Pool, s settings) (*authkit.Client, error) {
	email, err := smtp.New(smtp.Config{Server: s.SMTP, AppName: "OnlyDemo"})
	if err != nil {
		return nil, err
	}
	secret := sha256.Sum256([]byte(s.ClientSecret))
	return authkit.New(ctx, authkit.Config{
		Database:  authkit.DatabaseConfig{Schema: "profiles"},
		Issuer:    s.PublicURL,
		HTTP:      &authkit.HTTPConfig{DirectPeerIP: true},
		TwoFactor: iam.TwoFactorDisabled,

		// guide:standalone-issuer
		// OpenRails is a resource server this AuthKit mints tokens for.
		AuthorizationServer: authkit.AuthorizationServerConfig{
			Resources: []authkit.ResourceServerConfig{{
				ID: s.OpenRailsURL, // the tokens' aud
				// A customer's own billing; the merchant API.
				Scopes: []string{"openrails:self", "openrails:merchant"},
				// The most a token may carry.
				Permissions: backendPermissions,
			}},
			Clients: []authkit.OAuthClientConfig{{
				ID:         webClient, // public: the browser proves a DPoP key instead of a secret
				Origins:    []string{s.PublicURL},
				Resources:  []string{s.OpenRailsURL},
				GrantTypes: []authkit.OAuthGrantType{authkit.GrantTokenExchange},
			}, {
				ID:           backendClient, // confidential: this server
				SecretSHA256: hex.EncodeToString(secret[:]),
				Resources:    []string{s.OpenRailsURL},
				Permissions:  backendPermissions,
				GrantTypes:   []authkit.OAuthGrantType{authkit.GrantClientCredentials},
			}},
		},
		// guide:end
	}, authkit.Deps{Postgres: db, Email: email})
}

// newBilling is the remote handle: the same *openrails.Client as embedded,
// over HTTP, with a client-credentials token from this app's AuthKit. The
// server trusts that issuer for one merchant, the one every call acts on.
func newBilling(ctx context.Context, s settings) (*openrails.Client, error) {
	// guide:standalone-client
	tokens := (&clientcredentials.Config{
		ClientID: backendClient, ClientSecret: s.ClientSecret, TokenURL: s.PublicURL + "/oauth2/token",
		Scopes: []string{"openrails:merchant"}, EndpointParams: url.Values{"resource": {s.OpenRailsURL}},
	}).TokenSource(context.WithoutCancel(ctx)) // caches each token until it expires
	remote, err := openrails.NewRemote(s.OpenRailsURL, openrails.RemoteOptions{Credentials: map[billing.MerchantID]openrails.RemoteCredential{
		s.Merchant: {Token: func(context.Context) (string, error) {
			t, err := tokens.Token()
			if err != nil {
				return "", err
			}
			return t.AccessToken, nil
		}},
	}})
	if err != nil {
		return nil, err
	}
	return remote.Merchant(s.Merchant)
	// guide:end
}

// newApp builds the app: its AuthKit (started: its River workers run its
// jobs), the billing client and the router.
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
	if err := authkitgin.Mount(r, ak); err != nil { // sign-up and sign-in under /auth/v1, the token endpoint at /oauth2/token
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
		ClientSecret: os.Getenv("OPENRAILS_CLIENT_SECRET"),
		SMTP: smtp.Server{
			Host: cmp.Or(os.Getenv("EMAIL_SMTP_HOST"), "localhost"),
			Port: cmp.Or(atoi(os.Getenv("EMAIL_SMTP_PORT")), 1025),
			From: "OnlyDemo <hello@onlydemo.example>",
		},
	}
	merchant, err := billing.ParseMerchantID(os.Getenv("OPENRAILS_MERCHANT_ID"))
	if err != nil {
		return errors.New("OPENRAILS_MERCHANT_ID must be the merchant's ID")
	}
	s.Merchant = merchant
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

	if os.Getenv("EXAMPLE_CHECK_ONLY") != "" {
		_, err := bill.ListMeters(ctx, billing.PageRequest{Limit: 1}) // reaches the server as the app's merchant
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
