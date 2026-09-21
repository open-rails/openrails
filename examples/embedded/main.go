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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/pkg/billingauth"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, getenv func(string) string) (runErr error) {
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
	// there. Compose components before binding; extend the supplied config.
	runtime, err := embed.New(ctx, embed.Options{
		Config: &config.Config{
			Env: "development", TestMode: config.CredentialPostureSandbox,
			MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB,
			DB: &config.DBConfig{URL: dsn},
		},
		PGXPool: pool,
		River:   embed.RiverFromHost(),
	})
	if err != nil {
		return err
	}
	defer runtime.Close(context.WithoutCancel(ctx))
	jobs, err := runtime.BindRiver(ctx, pool, func(_ context.Context, cfg *river.Config) error {
		cfg.Queues[river.QueueDefault] = river.QueueConfig{MaxWorkers: 4}
		cfg.Queues[embed.QueueBilling] = river.QueueConfig{MaxWorkers: 4}
		return nil
	})
	if err != nil {
		return err
	}
	merchantID, err := runtime.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{})
	if err != nil {
		return err
	}
	defer jobs.StopAndCancel(context.WithoutCancel(ctx))
	if err := jobs.Start(ctx); err != nil {
		return err
	}
	loopsCtx, cancelLoops := context.WithCancel(ctx)
	loopsDone := make(chan error, 1)
	go func() {
		loopsDone <- runtime.RunWorkers(loopsCtx)
		cancelLoops() // a worker failure also stops HTTP
	}()
	defer func() {
		cancelLoops()
		if err := <-loopsDone; err != nil && !errors.Is(err, context.Canceled) {
			runErr = errors.Join(runErr, err)
		}
	}()

	client, err := runtime.Client(openrails.WithCurrency("USD"))
	if err != nil {
		return err
	}
	if err := client.Verify(ctx); err != nil {
		return err
	}
	if err := runtime.Ready(ctx); err != nil {
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
	handler, err := runtime.Handler(embed.MountOptions{
		MountPrefix: "/billing",
		RouteSets:   []embed.RouteSet{embed.RouteSetCheckout, embed.RouteSetWebhooks},
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
		<-loopsCtx.Done()
		_ = server.Shutdown(context.WithoutCancel(ctx))
	}()
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
