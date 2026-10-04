// Command multimerchant hosts one engine for several merchants, as
// OpenRails-SaaS does: no declared merchant, so each operation names its
// merchant, and OpenRails runs its own River fleet. OPENRAILS_DATABASE_URL and
// a comma-separated OPENRAILS_MERCHANT_IDS are required.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	dsn, ids := getenv("OPENRAILS_DATABASE_URL"), getenv("OPENRAILS_MERCHANT_IDS")
	if dsn == "" || ids == "" {
		return errors.New("OPENRAILS_DATABASE_URL and OPENRAILS_MERCHANT_IDS are required")
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	cfg := openrails.Config{TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly}
	if err := openrails.Migrate(ctx, db, cfg); err != nil {
		return err
	}
	bill, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: db})
	if err != nil {
		return err
	}
	defer bill.Close(context.WithoutCancel(ctx))
	if err := bill.Start(ctx); err != nil {
		return err
	}
	for _, raw := range strings.Split(ids, ",") {
		id, err := billing.ParseMerchantID(strings.TrimSpace(raw))
		if err != nil {
			return err
		}
		if _, err := bill.GetMerchantConfiguration(ctx, openrails.ForMerchantID(id)); err != nil {
			return err
		}
		log.Printf("merchant %s ready", id)
	}
	return nil
}
