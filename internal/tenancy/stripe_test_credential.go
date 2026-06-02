package tenancy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// defaultStripeBalanceCheck verifies a Stripe secret key works WITHOUT charging,
// by issuing a read-only GET /v1/balance against the Stripe API. A 2xx means the
// key authenticates; a 401/403 means it is invalid; other statuses surface as an
// error. OpenRails has no Stripe SDK dependency, so this uses a raw HTTP call (the
// same approach as the webhook thin-event hydration path).
func defaultStripeBalanceCheck(ctx context.Context, secretKey string) error {
	secretKey = strings.TrimSpace(secretKey)
	if secretKey == "" {
		return fmt.Errorf("tenancy: empty stripe secret key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.stripe.com/v1/balance", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("tenancy: stripe key rejected (%d)", resp.StatusCode)
	}
	return fmt.Errorf("tenancy: stripe balance check failed (%d)", resp.StatusCode)
}
