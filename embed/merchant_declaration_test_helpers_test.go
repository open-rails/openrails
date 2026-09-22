//go:build integration

package embed_test

import (
	"context"

	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/pkg/merchant"
)

func newDeclaredMerchant(ctx context.Context, opts embed.Options, slug string, config embed.MerchantConfig) (*embed.Runtime, merchant.ID, error) {
	opts.Merchant = &embed.MerchantDeclaration{Slug: slug, Config: config}
	runtime, err := embed.New(ctx, opts)
	if err != nil {
		return nil, merchant.ID{}, err
	}
	client, err := runtime.Client()
	if err != nil {
		_ = runtime.Close(ctx)
		return nil, merchant.ID{}, err
	}
	return runtime, client.MerchantID(), nil
}
