// Command embedded is the README's "How to Install (Embedded)" program: a
// creator site where users sign in with AuthKit, buy courses individually or
// as a bundle, and buy a monthly or yearly channel membership. newBilling,
// billingAuth and run are the README's code; newAuth is a development AuthKit.
//
// Run it from this directory with DATABASE_URL. It reads catalog.yaml and
// merchant.yaml: copy merchant.example.yaml to merchant.yaml and fill in your
// NMI gateway's IDs and keys (merchant.yaml is git-ignored). ADDR is the
// listen address (default :8080); EXAMPLE_CHECK_ONLY=1 boots, checks
// readiness and exits.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	authkitgin "github.com/open-rails/authkit/adapters/gin"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"

	"github.com/open-rails/openrails"
	openrailsgin "github.com/open-rails/openrails/adapters/gin"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// newAuth is a development AuthKit: open registration, ephemeral signing keys,
// and its own River client (ak.Start runs it). Its roles hold OpenRails'
// merchant permissions like any other: billing-staff, on the site's root
// group, holds every one a person may.
func newAuth(ctx context.Context, db *pgxpool.Pool) (*authkit.Client, error) {
	rbac := authkit.NewRoles()
	merchant := rbac.Persona("merchant")
	declare := func(p string) iam.Perm { // merchant:<resource>:<action>
		parts := strings.SplitN(p, ":", 3)
		return merchant.Permission(parts[1], parts[2])
	}
	var staff []iam.Grant
	for _, p := range openrails.Permissions() {
		staff = append(staff, declare(p))
	}
	for _, p := range openrails.MachinePermissions() {
		declare(p) // automation's API keys, never a person's
	}
	rbac.Root.Role("billing-staff", staff...)
	cfg := authkit.Config{
		Roles:        rbac,
		Schema:       "profiles",
		Token:        authkit.TokenConfig{Issuer: "http://localhost:8080", IssuedAudiences: []string{"onlydemo"}},
		Keys:         authkit.KeysConfig{AllowEphemeralDevKeys: true},
		HTTP:         &authkit.HTTPConfig{DirectPeerIP: true},
		Registration: authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen, Verification: iam.RegistrationVerificationNone},
		TwoFactor:    authkit.TwoFactorConfig{Mode: iam.TwoFactorDisabled},
	}
	if err := authkit.Migrate(ctx, db, cfg, authkit.MigrateOptions{}); err != nil {
		return nil, err
	}
	return authkit.New(ctx, cfg, authkit.Deps{Postgres: db})
}

func newBilling(ctx context.Context, db *pgxpool.Pool) (*openrails.Client, error) {
	// You, the seller, and your payment processor accounts (PSPs), declared in merchant.yaml.
	merchant, err := openrails.ReadMerchantFile("merchant.yaml")
	if err != nil {
		return nil, err
	}

	// What you sell. New applies each distinct catalog once, even across restarts.
	// Later edits through the Go client remain available and are not undone by a replay.
	products, err := catalog.ReadFile("catalog.yaml")
	if err != nil {
		return nil, err
	}

	cfg := openrails.Config{
		Schema:            "billing",                    // the Postgres schema OpenRails' tables go in
		TestMode:          openrails.Sandbox,            // Enforces that supplied PSP credentials must give access to test / sandbox environments only, or it throws an error
		ProviderWriteMode: openrails.ProviderWritesFull, // Set to ProviderWritesReadOnly to prevent any billing
		Merchant:          merchant,
		Catalog:           products,
	}

	// 1. Create or upgrade OpenRails' tables. Safe to run on every boot.
	if err := openrails.Migrate(ctx, db, cfg); err != nil {
		return nil, err
	}

	// 2. Build the billing engine. It has no logins of its own: your auth guards its
	// routes when you mount them.
	return openrails.New(ctx, cfg, openrails.Deps{Postgres: db}) // the same pool your app uses
}

// billingAuth guards OpenRails' routes with AuthKit's own middleware: each
// signed-in user is their own customer, and staff hold OpenRails' merchant
// permissions on the site's root group.
type billingAuth struct{ ak *authkit.Client }

func (a billingAuth) Required() func(http.Handler) http.Handler {
	return verify.RequireSession(a.ak) // a banned, deleted or signed-out user is refused at once
}

func (a billingAuth) RequirePermission(p string) func(http.Handler) http.Handler {
	perm, err := a.ak.Permission(p)
	if err != nil {
		panic(err) // a merchant permission your Roles did not register
	}
	return verify.RequirePermissionOn(a.ak, iam.RootGroup(), perm)
}

func (a billingAuth) Sensitive() func(http.Handler) http.Handler {
	return verify.Sensitive(a.ak) // signed in within 15 minutes, with the second factor
}

// Identity says who signed in: each user is their own subject and invoker,
// whether in a browser session or on a device key.
func (a billingAuth) Identity(ctx context.Context) (openrails.Identity, bool) {
	cl, ok := verify.ClaimsFromContext(ctx)
	if !ok || cl.Kind != iam.ActorUser || cl.UserID == "" {
		return openrails.Identity{}, false
	}
	credential := openrails.Credential{Kind: openrails.CredentialSession, ID: cl.SessionID}
	if cl.DeviceKeyID != "" {
		credential = openrails.Credential{Kind: openrails.CredentialDeviceKey, ID: cl.DeviceKeyID}
	}
	return openrails.Identity{
		Issuer: cl.Issuer, Subject: cl.UserID, SubjectKind: openrails.SubjectUser,
		Invoker: openrails.Invoker{Issuer: cl.Issuer, ID: cl.UserID}, Credential: credential,
		Email: cl.Email, Username: cl.Username, EmailVerified: cl.EmailVerified,
	}, true
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()

	ak, err := newAuth(ctx, db) // see AuthKit's README
	if err != nil {
		return err
	}
	defer ak.Close()
	bill, err := newBilling(ctx, db)
	if err != nil {
		return err
	}
	defer bill.Close(context.WithoutCancel(ctx))

	// Background work. OpenRails runs its own River workers: rebills, retries of
	// failed payments, invoices.
	if err := ak.Start(ctx); err != nil {
		return err
	}
	if err := bill.Start(ctx); err != nil {
		return err
	}

	r := gin.Default()
	if err := authkitgin.Mount(r, ak); err != nil { // sign-up and sign-in under /api/v1
		return err
	}
	// Billing. Processor webhooks are always mounted; pick the rest.
	auth := billingAuth{ak}
	err = openrailsgin.Mount(r, bill, openrails.Routes{
		Auth:         auth,                          // who may call each route: OpenRails asks it, by route
		Prefix:       "/billing",                    // the API is served at /billing/v1/*
		Storefront:   true,                          // anyone can browse products and prices, and pay a checkout
		Customers:    openrails.CustomerSelfService, // signed-in users manage their own purchases, subscriptions and cards at /me
		Merchant:     false,                         // your staff's API at /merchant (refunds, customers, catalog)
		CatalogEdits: false,                         // with Merchant, also let that API change the catalog; your Go code always can
	})
	if err != nil {
		return err
	}

	// Which entitlement unlocks each video. Buying course-101 or the bundle grants
	// course:101; either membership price grants channel:membership.
	contentAccess := map[string]string{
		"css-101":      "course:101",
		"tailwind-102": "course:102",
		"members-qa":   "channel:membership",
	}
	r.GET("/videos/:id", authkitgin.Required(ak), func(c *gin.Context) {
		id := c.Param("id")
		entitlement, exists := contentAccess[id]
		if !exists {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		claims, _ := ak.VerifyRequest(c.Request)
		customer, err := billing.ParseCustomerID(claims.UserID) // each AuthKit user is their own customer
		if err != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		held, err := bill.CheckEntitlements(c, customer, billing.CheckEntitlementsParams{Entitlements: []string{entitlement}, At: time.Now()})
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		if !held[entitlement] {
			c.JSON(http.StatusPaymentRequired, gin.H{"error": "access_required"})
			return
		}
		c.File("videos/" + id + ".mp4")
	})

	if os.Getenv("EXAMPLE_CHECK_ONLY") != "" {
		return bill.Ready(ctx)
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.WithoutCancel(ctx))
	}()
	return server.ListenAndServe()
}
