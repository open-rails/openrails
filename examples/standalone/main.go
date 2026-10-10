// Command standalone calls an OpenRails server with a merchant credential:
// self-hosted, an access token from the merchant's trusted issuer (client
// credentials); on a hosted product, its API key. OPENRAILS_URL,
// OPENRAILS_TOKEN and OPENRAILS_MERCHANT_ID are required.
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
	baseURL, token := getenv("OPENRAILS_URL"), getenv("OPENRAILS_TOKEN")
	if baseURL == "" || token == "" {
		return errors.New("OPENRAILS_URL and OPENRAILS_TOKEN are required")
	}
	merchantID, err := billing.ParseMerchantID(getenv("OPENRAILS_MERCHANT_ID"))
	if err != nil {
		return err
	}
	client, err := openrails.NewRemote(baseURL,
		// A real backend mints the token per call (WithTokenProvider over its
		// issuer's client-credentials grant).
		openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil }),
		openrails.WithMerchantID(merchantID),
	)
	if err != nil {
		return err
	}
	// One authenticated read proves the server, the credential and the merchant.
	if _, err := client.GetMerchantConfiguration(ctx); err != nil {
		return err
	}
	log.Printf("OpenRails server ready for merchant %s", merchantID)
	return nil
}
