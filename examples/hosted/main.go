// Command hosted is the embedded example's course site on the hosted
// platform: OpenRails runs there, and this app is one merchant's. The backend
// calls the platform with the console's API key and, for each signed-in user,
// mints a short customer token the React app's billing-ui sends to the
// platform. content.go and web/src/pages.tsx are the embedded example's.
//
// Run it from this directory with DATABASE_URL (this app's own users), after
// building the app (cd web && pnpm install && pnpm build) and setting up the
// merchant in the console (README.md): OPENRAILS_URL is the API URL,
// OPENRAILS_MERCHANT_ID the merchant's id, OPENRAILS_API_KEY a merchant API
// key, OPENRAILS_TOKEN_URL the customer-token endpoint, and
// OPENRAILS_SIGNING_KEY a PEM private key registered for customer tokens
// under OPENRAILS_KEY_ID, with PUBLIC_URL (default http://localhost:8080) as
// its issuer. `go run . catalog apply catalog.yaml` applies the catalog;
// starting the app never does. ADDR is the listen address (default :8080);
// EXAMPLE_CHECK_ONLY=1 boots, checks the platform and exits.
package main

import (
	"cmp"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
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
	"github.com/open-rails/authkit/verify"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// settings are the app's deployment: where it is, its merchant on the
// platform, and its mail server.
type settings struct {
	PublicURL  string             // this app: AuthKit's issuer, the browser's origin, the customer tokens' issuer
	APIURL     string             // the platform's API URL, without /v1
	Merchant   billing.MerchantID // the merchant this app sells for
	APIKey     string             // a merchant API key from the console
	TokenURL   string             // where customer-token assertions are redeemed
	KeyID      string             // the kid the signing key is registered under
	SigningKey crypto.Signer      // the private half of the key registered for customer tokens
	SMTP       smtp.Server        // AuthKit's mail: the codes that prove each user's email
}

// newAuth is a development AuthKit for the app's own users. Users prove their
// email: the customer token carries only a proven address, and the platform
// sends receipts to it.
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
		Keys:            authkit.KeysConfig{AllowEphemeralDevKeys: true},
		HTTP:            &authkit.HTTPConfig{DirectPeerIP: true, RefreshCookie: true},
		Registration:    authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen, Verification: iam.RegistrationVerificationRequired},
		TwoFactor:       authkit.TwoFactorConfig{Mode: iam.TwoFactorDisabled},
	}, authkit.Deps{Postgres: db, Email: email})
}

// newBilling is the merchant's handle on the platform: the same
// *openrails.Client as embedded, over HTTP with the API key. The signing key
// mints customer tokens.
func newBilling(s settings) (*openrails.Client, error) {
	remote, err := openrails.NewRemote(s.APIURL, openrails.RemoteOptions{
		Credentials: map[billing.MerchantID]openrails.RemoteCredential{s.Merchant: {
			APIKey: s.APIKey,
			Assertion: &openrails.AssertionKey{
				Issuer: s.PublicURL, KeyID: s.KeyID, Signer: s.SigningKey, TokenURL: s.TokenURL,
			},
		}},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return remote.Merchant(s.Merchant)
}

// billingRoutes are what the browser needs from this server to buy: where
// the platform is, a customer token for the signed-in user, and a hosted
// checkout for one price.
func billingRoutes(r *gin.Engine, ak *authkit.Client, bill *openrails.Client, s settings) {
	r.GET("/api/billing", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"base_url": s.APIURL + "/v1"})
	})
	// guide:hosted-token
	r.POST("/api/billing/token", authkitgin.Required(ak), func(c *gin.Context) {
		claims, _ := verify.ClaimsFromContext(c.Request.Context())
		customer, err := billing.ParseCustomerID(claims.UserID) // each user is their own customer
		if err != nil {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		token, err := bill.CustomerToken(c, billing.CustomerTokenParams{
			CustomerID: customer, Email: claims.Email, EmailVerified: claims.EmailVerified,
		})
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.JSON(http.StatusOK, gin.H{"access_token": token.AccessToken, "expires_in": int(time.Until(token.ExpiresAt).Seconds())})
	})
	// guide:end
	// POST /api/checkout {"price_id"} answers the pay URL of a new order for
	// the signed-in user. The browser makes the Idempotency-Key when the
	// buyer decides and resends it on every retry.
	r.POST("/api/checkout", authkitgin.Required(ak), func(c *gin.Context) {
		var body struct {
			PriceID string `json:"price_id"`
		}
		key := c.GetHeader("Idempotency-Key")
		claims, _ := verify.ClaimsFromContext(c.Request.Context())
		customer, err := billing.ParseCustomerID(claims.UserID)
		if c.BindJSON(&body) != nil || key == "" || err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		price, err := billing.ParsePriceID(body.PriceID)
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		order, err := bill.CreateOrder(c, key, billing.CreateOrderParams{
			CustomerID: customer,
			Lines:      []billing.OrderLineParams{{PriceID: price}},
			PayURL: &billing.OrderPayURLParams{
				SuccessURL: s.PublicURL + "/?order={ORDER_ID}", // access follows the order, not this redirect
				CancelURL:  new(s.PublicURL + "/"),
			},
		})
		if err != nil || order.PayURL == nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.JSON(http.StatusOK, gin.H{"pay_url": *order.PayURL})
	})
}

// newApp builds the app: its AuthKit, the billing client and the router.
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
	if err := authkitgin.Mount(r, ak); err != nil { // sign-up and sign-in under /api/v1
		_ = ak.Close(context.WithoutCancel(ctx))
		return nil, nil, nil, err
	}
	billingRoutes(r, ak, bill, s)
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
	s, err := load()
	if err != nil {
		return err
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
		_, err := bill.ListMeters(ctx, billing.PageRequest{Limit: 1}) // reaches the platform as the merchant
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

// load reads the settings from the environment.
func load() (settings, error) {
	s := settings{
		PublicURL: cmp.Or(os.Getenv("PUBLIC_URL"), "http://localhost:8080"),
		APIURL:    os.Getenv("OPENRAILS_URL"),
		APIKey:    os.Getenv("OPENRAILS_API_KEY"),
		TokenURL:  os.Getenv("OPENRAILS_TOKEN_URL"),
		KeyID:     os.Getenv("OPENRAILS_KEY_ID"),
		SMTP: smtp.Server{
			Host: cmp.Or(os.Getenv("EMAIL_SMTP_HOST"), "localhost"),
			Port: cmp.Or(atoi(os.Getenv("EMAIL_SMTP_PORT")), 1025),
			From: "OnlyDemo <hello@onlydemo.example>",
		},
	}
	if s.APIURL == "" || s.APIKey == "" || s.TokenURL == "" || s.KeyID == "" {
		return s, errors.New("OPENRAILS_URL, OPENRAILS_MERCHANT_ID, OPENRAILS_API_KEY, OPENRAILS_TOKEN_URL, OPENRAILS_KEY_ID and OPENRAILS_SIGNING_KEY are required: see README.md")
	}
	merchant, err := billing.ParseMerchantID(os.Getenv("OPENRAILS_MERCHANT_ID"))
	if err != nil {
		return s, err
	}
	s.Merchant = merchant
	s.SigningKey, err = readSigner(os.Getenv("OPENRAILS_SIGNING_KEY"))
	return s, err
}

// readSigner reads a PKCS #8 PEM private key: ECDSA, Ed25519 or RSA.
func readSigner(path string) (crypto.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New(path + ": no PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New(path + ": not a signing key")
	}
	return signer, nil
}

// atoi is s as a number, 0 when it is not one.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
