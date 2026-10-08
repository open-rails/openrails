// Command embedded is the README's "How to Install (Embedded)" program: a
// video site where users sign in with AuthKit and buy a monthly premium plan
// or an individual video. newBilling and run are
// the README's code; newAuth is a development AuthKit.
//
// Run it from this directory (it reads catalog.yaml) with DATABASE_URL and the
// PSP's MOBIUS_ACCOUNT_ID, MOBIUS_SECURITY_KEY and MOBIUS_WEBHOOK_SIGNING_SECRET
// (MOBIUS_RAIL=nmi). ADDR is the listen address (default :8080);
// EXAMPLE_CHECK_ONLY=1 boots, checks readiness and exits.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	authkitgin "github.com/open-rails/authkit/adapters/gin"
	"github.com/open-rails/authkit/iam"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails"
	openrailsgin "github.com/open-rails/openrails/adapters/gin"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// newAuth is a development AuthKit: open registration, ephemeral signing keys,
// and a River fleet the host owns (shared with OpenRails below).
func newAuth(ctx context.Context, db *pgxpool.Pool) (*authkit.Client, error) {
	cfg := authkit.Config{
		Schema:       "profiles",
		Token:        authkit.TokenConfig{Issuer: "http://localhost:8080", IssuedAudiences: []string{"myvideos"}},
		Keys:         authkit.KeysConfig{AllowEphemeralDevKeys: true},
		HTTP:         &authkit.HTTPConfig{DirectPeerIP: true},
		Registration: authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen, Verification: iam.RegistrationVerificationNone},
		TwoFactor:    authkit.TwoFactorConfig{Mode: iam.TwoFactorDisabled},
		River:        authkit.RiverConfig{HostOwned: true},
	}
	if err := authkit.Migrate(ctx, db, cfg, authkit.MigrateOptions{}); err != nil {
		return nil, err
	}
	return authkit.New(ctx, cfg, authkit.Deps{Postgres: db})
}

func newBilling(ctx context.Context, db *pgxpool.Pool, auth *authkit.Client) (*openrails.Client, error) {
	// Your payment processor account (a PSP). This one is an NMI gateway named "mobius", read
	// from MOBIUS_RAIL=nmi, MOBIUS_ACCOUNT_ID and MOBIUS_SECURITY_KEY. Declare as many as you like.
	mobius, err := openrails.PSPFromEnv("mobius", os.LookupEnv)
	if err != nil {
		return nil, err
	}

	// What you sell. Each distinct catalog batch is applied once, even across restarts.
	// Later programmatic edits remain available and are not undone by a replay.
	raw, err := os.ReadFile("catalog.yaml")
	if err != nil {
		return nil, err
	}
	declared, err := catalog.ParseApplicationYAML(raw)
	if err != nil {
		return nil, err
	}

	cfg := openrails.Config{
		Schema:            "billing",                    // the Postgres schema OpenRails' tables go in
		TestMode:          openrails.Sandbox,            // Sandbox or Live: which PSP credentials are accepted
		ProviderWriteMode: openrails.ProviderWritesFull, // ProviderWritesReadOnly never charges anyone
		Merchant: openrails.MerchantDeclaration{
			Slug: "myvideos", // you, the seller
			PSPs: map[string]openrails.PSPConfig{"mobius": mobius},
		},
		AllowCatalogUpdates: false, // hide catalog-write HTTP routes; the Go client can still edit
		HTTP: &openrails.HTTPConfig{
			Merchant: true,                        // authenticated merchant routes; catalog HTTP writes stay disabled
			Checkout: &openrails.CheckoutConfig{}, // products, prices, checkout sessions and processor webhooks
			CustomerRoutes: []openrails.CustomerRoutesConfig{
				{Scope: openrails.CustomerSelfService}, // /v1/me/*: users manage their own subscriptions and cards
			},
		},
		River: openrails.RiverHostOwned, // renewals, dunning and invoices run on your River workers
	}

	// 1. Create or upgrade OpenRails' tables. Safe to run on every boot.
	if err := openrails.Migrate(ctx, db, cfg); err != nil {
		return nil, err
	}

	// 2. Build the billing engine. OpenRails has no logins of its own: it asks your AuthKit
	// who is calling, and each user is their own paying customer.
	client, err := openrails.New(ctx, cfg, openrails.Deps{
		Postgres: db,   // required: the same pool your app uses
		AuthKit:  auth, // who is calling, what staff may do, and how recently they signed in
	})
	if err != nil {
		return nil, err
	}
	if _, err := client.ApplyCatalog(ctx, declared); err != nil {
		_ = client.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return client, nil
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

	auth, err := newAuth(ctx, db) // see AuthKit's README
	if err != nil {
		return err
	}
	defer auth.Close()
	bill, err := newBilling(ctx, db, auth)
	if err != nil {
		return err
	}
	defer bill.Close(ctx)

	// One River worker fleet runs your jobs, AuthKit's and OpenRails' (rebills, retries, invoices).
	// The fleet is yours, so you create River's tables ("" is River's default schema, public).
	if err := riverhelpers.ApplyMigrations(ctx, db, ""); err != nil {
		return err
	}
	workers, err := riverhelpers.New(ctx, db, &river.Config{}, auth.RiverJobs(), bill.RiverJobs())
	if err != nil {
		return err
	}
	if err := workers.Start(ctx); err != nil {
		return err
	}
	defer workers.StopAndCancel(context.WithoutCancel(ctx))
	if err := auth.Start(ctx); err != nil {
		return err
	}
	if err := bill.Start(ctx); err != nil {
		return err
	}

	r := gin.Default()
	if err := authkitgin.Mount(r, auth); err != nil { // sign-up and sign-in under /api/v1
		return err
	}
	if err := openrailsgin.Mount(r.Group("/billing"), bill); err != nil { // billing under /billing/v1
		return err
	}

	// Premium members can watch any video; a one-off buyer can watch the video they bought.
	r.GET("/videos/:id", authkitgin.Required(auth), func(c *gin.Context) {
		claims, _ := auth.VerifyRequest(c.Request)
		customer, err := billing.ParseCustomerID(claims.UserID)
		if err != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		premium, err := bill.HasEntitlement(c, customer, "premium", time.Now())
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		if !premium {
			productKey := "video-" + c.Param("id")
			access, err := bill.CheckProductAccess(c, customer, billing.CheckProductAccessParams{ProductKeys: []string{productKey}})
			if err != nil {
				c.AbortWithStatus(http.StatusServiceUnavailable)
				return
			}
			if !access[productKey] {
				c.JSON(http.StatusPaymentRequired, gin.H{"error": "purchase_required"})
				return
			}
		}
		c.File("videos/" + c.Param("id") + ".mp4")
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
