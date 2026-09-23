// Command multimerchant hosts one engine for several merchants, as
// OpenRails-SaaS does. Each merchant gets its own immutably bound client; the
// engine owns and runs its River fleet. OPENRAILS_DATABASE_URL and a
// comma-separated OPENRAILS_MERCHANT_IDS are required.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/pkg/merchant"
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
	runtime, err := embed.New(ctx, embed.Options{
		Config: &config.Config{
			TestMode:           config.CredentialPostureSandbox,
			MerchantConfigHTTP: true, SecretBackend: config.SecretBackendDB,
			DB: &config.DBConfig{URL: dsn},
		},
		River: embed.RiverManagedByOpenRails(),

		RunWorkers: true,
	})
	if err != nil {
		return err
	}
	defer runtime.Close(context.WithoutCancel(ctx))

	clients := map[merchant.ID]*openrails.Client{}
	for _, raw := range strings.Split(ids, ",") {
		id, err := merchant.ParseID(strings.TrimSpace(raw))
		if err != nil {
			return err
		}
		client, err := runtime.Client(openrails.WithMerchantID(id), openrails.WithCurrency("USD"))
		if err != nil {
			return err
		}
		if err := client.Verify(ctx); err != nil {
			return err
		}
		clients[id] = client
	}
	for id, client := range clients {
		if _, err := client.GetMerchantSettings(ctx); err != nil {
			return err
		}
		log.Printf("merchant %s ready", id)
	}
	return nil
}
