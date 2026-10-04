// Command standalone calls an OpenRails server (self-hosted or OpenRails-SaaS)
// with a merchant API key. OPENRAILS_URL, OPENRAILS_API_KEY and
// OPENRAILS_MERCHANT_ID are required.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

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
	baseURL, key := getenv("OPENRAILS_URL"), getenv("OPENRAILS_API_KEY")
	if baseURL == "" || key == "" {
		return errors.New("OPENRAILS_URL and OPENRAILS_API_KEY are required")
	}
	merchantID, err := billing.ParseMerchantID(getenv("OPENRAILS_MERCHANT_ID"))
	if err != nil {
		return err
	}
	client, err := openrails.NewRemote(baseURL,
		openrails.WithAPIKey(key),
		openrails.WithMerchantID(merchantID),
	)
	if err != nil {
		return err
	}
	// One authenticated read proves the server, the key and the merchant.
	if _, err := client.GetMerchantConfiguration(ctx); err != nil {
		return err
	}
	log.Printf("OpenRails server ready for merchant %s", merchantID)
	return nil
}
