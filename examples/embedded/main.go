// Command embedded runs OpenRails inside a host process. The host owns the
// database pool, the River client and HTTP serving; billing code uses the same
// *openrails.Client a remote deployment uses.
//
// Apply migrations first (openrails migrate) and connect as the unprivileged
// NOBYPASSRLS role. OPENRAILS_DATABASE_URL and OPENRAILS_MERCHANT are required;
// OPENRAILS_EXAMPLE_ADDR serves the mounted checkout and webhook routes.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	dsn, slug := getenv("OPENRAILS_DATABASE_URL"), getenv("OPENRAILS_MERCHANT")
	if dsn == "" || slug == "" {
		return errors.New("OPENRAILS_DATABASE_URL and OPENRAILS_MERCHANT are required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	// River is mandatory: renewals, dunning, invoices and reconciliation run
	// there. The host adds its own workers to fleet.Workers before building.
	var jobs *river.Client[pgx.Tx]
	runtime, err := embed.New(ctx, embed.Options{Options: embedded.Options{
		Config: &config.Config{
			Env: "development", TestMode: config.CredentialPostureSandbox,
			MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB,
			DB: &config.DBConfig{URL: dsn},
		},
		PGXPool: pool,
		River: embedded.RiverFromHost(func(_ context.Context, fleet *embedded.RiverFleet) (*river.Client[pgx.Tx], error) {
			jobs, err = river.NewClient(riverpgxv5.New(pool), &river.Config{
				Workers: fleet.Workers,
				Schema:  fleet.Schema,
				Queues: map[string]river.QueueConfig{
					river.QueueDefault: {MaxWorkers: 4},
					fleet.QueueBilling: {MaxWorkers: 4},
				},
			})
			return jobs, err
		}),
	}})
	if err != nil {
		return err
	}
	defer runtime.Close(context.WithoutCancel(ctx))
	merchantID, err := runtime.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{})
	if err != nil {
		return err
	}
	if err := jobs.Start(ctx); err != nil {
		return err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = jobs.Stop(stopCtx)
	}()

	client, err := runtime.Client(openrails.WithCurrency("USD"))
	if err != nil {
		return err
	}
	if err := client.Verify(ctx); err != nil {
		return err
	}
	if err := runtime.Embedded().Ready(ctx); err != nil {
		return err
	}
	if _, err := client.GetMerchantSettings(ctx); err != nil {
		return err
	}
	log.Printf("embedded OpenRails ready for merchant %s", merchantID)

	addr := getenv("OPENRAILS_EXAMPLE_ADDR")
	if addr == "" {
		return nil
	}
	handler, err := embedded.MountHandler(runtime.Embedded(), embedded.MountOptions{
		MountPrefix: "/billing",
		RouteSets:   []embedded.RouteSet{embedded.RouteSetCheckout, embedded.RouteSetWebhooks},
		// Replace with the host's session verifier; subjects must be UUIDs.
		Authenticator: billingauth.AuthenticatorFunc(func(context.Context, *http.Request) (billingauth.UserContext, error) {
			return billingauth.UserContext{}, fmt.Errorf("sign in required")
		}),
	})
	if err != nil {
		return err
	}
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.WithoutCancel(ctx))
	}()
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
