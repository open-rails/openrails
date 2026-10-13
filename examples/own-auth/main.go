// Command own-auth is a shop at https://shop.example selling for two
// merchants on one OpenRails engine, with its own authentication: its own users,
// sessions and roles (schema.sql) answer OpenRails through the
// Authenticator in auth.go. It is a backend only; its tests drive it.
//
// Run it with DATABASE_URL naming a PostgreSQL 18 database; ADDR is the
// listen address (default :8080).
package main

import (
	"cmp"
	"context"
	_ "embed"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
)

//go:embed schema.sql
var schema string

// shop is the host: its auth, the billing engine and the routes it serves.
type shop struct {
	auth      shopAuth
	bill      *openrails.Engine
	routes    openrails.Routes
	merchants map[string]billing.MerchantID // by slug
	handler   http.Handler
}

func newShop(ctx context.Context, db *pgxpool.Pool) (*shop, error) {
	if _, err := db.Exec(ctx, schema); err != nil {
		return nil, err
	}
	// Two merchants on one engine and no Issuer: each request acts at the
	// merchant its session's BoundScope names.
	bill, err := openrails.New(ctx, openrails.Config{
		Database:          openrails.DatabaseConfig{Schema: "billing"},
		ProviderWriteMode: openrails.ProviderWritesFull,
		Merchants: map[string]openrails.MerchantDeclaration{
			"acme":   {DisplayName: "Acme"},
			"globex": {DisplayName: "Globex"},
		},
	}, openrails.Deps{Postgres: db, UserInfo: directory{db}})
	if err != nil {
		return nil, err
	}
	s := &shop{auth: shopAuth{db}, bill: bill, merchants: map[string]billing.MerchantID{}}
	fail := func(err error) (*shop, error) {
		_ = bill.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	var ids []billing.MerchantID
	for _, slug := range []string{"acme", "globex"} {
		handle, err := bill.MerchantBySlug(slug)
		if err != nil {
			return fail(err)
		}
		s.merchants[slug] = handle.MerchantID()
		ids = append(ids, handle.MerchantID())
	}

	mux := http.NewServeMux()
	s.routes = openrails.Routes{
		Auth:        s.auth, // says who a request is and what they hold; OpenRails decides what to admit
		Prefix:      "/billing",
		RouteGroups: openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true},
	}
	if err := openrailshttp.Mount(mux, bill, s.routes); err != nil {
		return fail(err)
	}
	mux.HandleFunc("POST /switch", s.auth.switchMerchant(ids))
	mux.HandleFunc("POST /sign-out", s.auth.signOut)
	s.handler = mux
	return s, nil
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
	s, err := newShop(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = s.bill.Close(context.WithoutCancel(ctx)) }()
	if err := s.bill.Start(ctx); err != nil {
		return err
	}
	server := &http.Server{Addr: cmp.Or(os.Getenv("ADDR"), ":8080"), Handler: s.handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.WithoutCancel(ctx))
	}()
	return server.ListenAndServe()
}
