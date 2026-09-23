package merchants

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// defaultStripeBalanceCheck verifies a Stripe secret key works WITHOUT charging,
// by issuing a read-only GET /v1/balance against the Stripe API. A 2xx means the
// key authenticates; a 401/403 means it is invalid; other statuses surface as an
// error. OpenRails has no Stripe SDK dependency, so this uses a raw HTTP call (the
// same approach as the webhook thin-event hydration path).
func defaultStripeBalanceCheck(ctx context.Context, secretKey string) error {
	return stripeBalanceCheck(ctx, secretKey, nil)
}

func stripeBalanceCheck(ctx context.Context, secretKey string, clients *stripeapi.Factory) error {
	secretKey = strings.TrimSpace(secretKey)
	if secretKey == "" {
		return apperr.Invalidf("merchants: empty stripe secret key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.stripe.com/v1/balance", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	// Pure read path with no config at hand: the unconditionally write-blocked
	// choke client keeps the GET working in every mode while making any future
	// mutation here fail loudly.
	resp, err := clients.ReadOnlyClient(15 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: stripe key rejected (%d)", ErrPaymentProviderCredentialsRejected, resp.StatusCode)
	}
	return fmt.Errorf("merchants: stripe balance check failed (%d)", resp.StatusCode)
}

// stripeAccountCheck binds a candidate to the authenticating Stripe account and
// credential environment before publication. Account objects have no livemode;
// the authenticated balance read supplies that fact. Neither request mutates Stripe.
func stripeAccountCheck(ctx context.Context, key, environment, accountID string, clients *stripeapi.Factory) error {
	key = strings.TrimSpace(key)
	if environment != "test" && environment != "live" {
		return fmt.Errorf("%w: invalid Stripe credential environment", ErrPaymentProviderCredentialsRejected)
	}
	if !strings.HasPrefix(key, "sk_"+environment+"_") && !strings.HasPrefix(key, "rk_"+environment+"_") {
		return fmt.Errorf("%w: Stripe key does not match credential environment", ErrPaymentProviderCredentialsRejected)
	}
	client := clients.ReadOnlyClient(providerCredentialProbeTimeout)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	read := func(path string, target any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.stripe.com"+path, nil)
		if err != nil {
			return fmt.Errorf("%w: build Stripe identity probe", ErrSecretBackendUnavailable)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("%w: Stripe identity verification unavailable", ErrSecretBackendUnavailable)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%w: Stripe identity verification denied", ErrPaymentProviderCredentialsRejected)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("%w: Stripe identity verification failed (%d)", ErrSecretBackendUnavailable, resp.StatusCode)
		}
		const maxBody = 1 << 20
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil || len(body) > maxBody || json.Unmarshal(body, target) != nil {
			return fmt.Errorf("%w: invalid Stripe identity verification response", ErrSecretBackendUnavailable)
		}
		return nil
	}
	var account struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	if err := read("/v1/account", &account); err != nil {
		return err
	}
	if account.Object != "account" || account.ID != accountID || !strings.HasPrefix(account.ID, "acct_") {
		return fmt.Errorf("%w: Stripe credential account does not match declared account", ErrPaymentProviderCredentialsRejected)
	}
	var balance struct {
		Object   string `json:"object"`
		LiveMode *bool  `json:"livemode"`
	}
	if err := read("/v1/balance", &balance); err != nil {
		return err
	}
	if balance.Object != "balance" || balance.LiveMode == nil || *balance.LiveMode != (environment == "live") {
		return fmt.Errorf("%w: Stripe credential environment does not match declared environment", ErrPaymentProviderCredentialsRejected)
	}
	return nil
}
