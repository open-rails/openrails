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
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
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
		Merchant: &embed.MerchantDeclaration{Slug: slug},
		Auth: &billingauth.Integration{Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
			return billingauth.Identity{}, billingauth.ErrUnauthenticated
		})},
		HTTP: &embed.HTTPConfig{Checkout: true},
		Config: &config.Config{
			TestMode: config.CredentialPostureSandbox,
			DB:       &config.DBConfig{URL: dsn},
		},
		PGXPool: pool,
		River:   embed.RiverFromHost(),
	})
	if err != nil {
		return err
	}
	defer runtime.Close(context.WithoutCancel(ctx))
	jobs, err := riverhelpers.New(ctx, pool, &river.Config{Queues: map[string]river.QueueConfig{embed.QueueBilling: {MaxWorkers: 4}}}, runtime.RiverJobs())
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
	log.Printf("embedded OpenRails ready for merchant %s", client.MerchantID())

	addr := getenv("OPENRAILS_EXAMPLE_ADDR")
	if addr == "" {
		return nil
	}
	routes, err := openrailshttp.Routes(runtime)
	if err != nil {
		return err
	}
	handler := http.NewServeMux()
	if err := routes.Mount(handler, "/billing"); err != nil {
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
