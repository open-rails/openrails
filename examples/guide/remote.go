package guide

import (
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// Remote is a merchant's handle over HTTP, on a standalone server or the
// hosted platform, with a merchant API key.
func Remote(apiURL string, merchant billing.MerchantID, apiKey string, rdb redis.UniversalClient) (*openrails.Client, error) {
	// guide:remote
	remote, err := openrails.NewRemote(apiURL, openrails.RemoteOptions{ // the API URL, without /v1
		Credentials: map[billing.MerchantID]openrails.RemoteCredential{merchant: {APIKey: apiKey}},
		Timeout:     10 * time.Second,
		EntitlementCheckCache: openrails.EntitlementCheckCacheOptions{
			Redis: rdb, // optional: replicas share answers; nil keeps them in memory
		},
	})
	if err != nil {
		return nil, err
	}
	return remote.Merchant(merchant) // every call checks the server answered for this merchant
	// guide:end
}
