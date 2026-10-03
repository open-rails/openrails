// Command embedded runs OpenRails inside a host process, as the README's
// "How to Install (Embedded)" does: the host owns the Postgres pool, the River
// fleet and HTTP serving, and billing code uses the same *openrails.Client a
// remote deployment uses.
//
// OPENRAILS_DATABASE_URL is required. OPENRAILS_EXAMPLE_ADDR serves the
// mounted routes under /billing until interrupted; without it the command
// checks the engine and exits. Authentication here is a stand-in: the bearer
// token is the user's UUID. Use your identity provider instead.
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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
)

const catalog = `schema_version: 1
products:
  - key: premium
    display_name: Premium
    entitlements_spec: {premium: null}
    prices:
      - key: premium-monthly
        currency: USD
        unit_amount: 9990000
        access_duration_hours: 720
        auto_renew: true
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	dsn := getenv("OPENRAILS_DATABASE_URL")
	if dsn == "" {
		return errors.New("OPENRAILS_DATABASE_URL is required")
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	cfg := openrails.Config{
		Schema:              "billing",
		TestMode:            openrails.Sandbox,
		ProviderWriteMode:   openrails.ProviderWritesReadOnly,
		AllowCatalogUpdates: true,
		Merchant:            openrails.MerchantDeclaration{Slug: "example"},
		HTTP: &openrails.HTTPConfig{
			Checkout:       true,
			CustomerRoutes: []openrails.CustomerRoutesConfig{{Scope: openrails.CustomerSelfService}},
		},
		River: openrails.RiverHostOwned,
	}
	if err := openrails.Migrate(ctx, db, cfg); err != nil {
		return err
	}
	bill, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: db, Authenticate: authenticate})
	if err != nil {
		return err
	}
	defer bill.Close(context.WithoutCancel(ctx))

	params, err := billing.ParseCatalogApplicationYAML([]byte(catalog))
	if err != nil {
		return err
	}
	if _, err := bill.Catalog.Apply(ctx, params); err != nil {
		return err
	}

	// One River fleet runs the host's jobs and OpenRails' (and AuthKit's, when
	// the host runs AuthKit). A host-owned fleet migrates River itself.
	if err := riverhelpers.ApplyMigrations(ctx, db, ""); err != nil {
		return err
	}
	workers, err := riverhelpers.New(ctx, db, &river.Config{}, bill.RiverJobs())
	if err != nil {
		return err
	}
	if err := workers.Start(ctx); err != nil {
		return err
	}
	defer workers.StopAndCancel(context.WithoutCancel(ctx))
	if err := bill.Start(ctx); err != nil {
		return err
	}
	if err := bill.Ready(ctx); err != nil {
		return err
	}

	mux := http.NewServeMux()
	if err := openrailshttp.Mount(mux, bill, "/billing"); err != nil {
		return err
	}
	premium, err := bill.HasEntitlement(ctx, uuid.NewString(), "premium", time.Now())
	if err != nil {
		return err
	}
	log.Printf("embedded OpenRails ready for merchant %s (a new user has premium: %t)", bill.MerchantID(), premium)

	addr := getenv("OPENRAILS_EXAMPLE_ADDR")
	if addr == "" {
		return nil
	}
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.WithoutCancel(ctx))
	}()
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// authenticate is a stand-in for the host's identity provider: the bearer
// token is the user's UUID, and each user pays for themselves.
func authenticate(r *http.Request) (openrails.Identity, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if _, err := uuid.Parse(token); !ok || err != nil {
		return openrails.Identity{}, openrails.ErrUnauthenticated
	}
	return openrails.Identity{Kind: openrails.User, Issuer: "https://example.invalid", SubjectID: token, CustomerID: token}, nil
}
