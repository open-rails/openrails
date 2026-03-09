package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/config"
)

type StripeCatalogService struct {
	Config *config.Config
}

type stripeObject struct {
	ID string `json:"id"`
}

func (s *StripeCatalogService) CreateProduct(ctx context.Context, name, description, idempotencyKey string) (string, error) {
	stripeProc := s.Config.GetStripeProcessor()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return "", fmt.Errorf("stripe is not configured")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("stripe product name required")
	}

	form := url.Values{}
	form.Set("name", name)
	if strings.TrimSpace(description) != "" {
		form.Set("description", description)
	}

	obj, err := stripePostForm(ctx, stripeProc.SecretKey, "https://api.stripe.com/v1/products", form, idempotencyKey)
	if err != nil {
		return "", err
	}
	return obj.ID, nil
}

func (s *StripeCatalogService) CreatePrice(ctx context.Context, stripeProductID string, unitAmount int64, currency string, billingCycleDays *int, idempotencyKey string) (string, error) {
	stripeProc := s.Config.GetStripeProcessor()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return "", fmt.Errorf("stripe is not configured")
	}
	stripeProductID = strings.TrimSpace(stripeProductID)
	if stripeProductID == "" {
		return "", fmt.Errorf("stripe_product_id required")
	}
	if unitAmount <= 0 {
		return "", fmt.Errorf("unit_amount must be positive")
	}
	currency = strings.ToLower(strings.TrimSpace(currency))
	if currency == "" {
		return "", fmt.Errorf("currency required")
	}

	form := url.Values{}
	form.Set("product", stripeProductID)
	form.Set("unit_amount", strconv.FormatInt(unitAmount, 10))
	form.Set("currency", currency)

	// recurring price for subscriptions
	if billingCycleDays != nil && *billingCycleDays > 0 {
		interval, intervalCount := stripeIntervalForDays(*billingCycleDays)
		form.Set("recurring[interval]", interval)
		if intervalCount > 1 {
			form.Set("recurring[interval_count]", strconv.Itoa(intervalCount))
		}
	}

	obj, err := stripePostForm(ctx, stripeProc.SecretKey, "https://api.stripe.com/v1/prices", form, idempotencyKey)
	if err != nil {
		return "", err
	}
	return obj.ID, nil
}

func (s *StripeCatalogService) VerifyPriceExists(ctx context.Context, priceID string) error {
	stripeProc := s.Config.GetStripeProcessor()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return fmt.Errorf("stripe is not configured")
	}
	priceID = strings.TrimSpace(priceID)
	if priceID == "" {
		return fmt.Errorf("stripe price_id required")
	}

	endpoint := "https://api.stripe.com/v1/prices/" + url.PathEscape(priceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+stripeProc.SecretKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("stripe price verification failed: %s", parseStripeError(body))
	}
	return nil
}

func stripeIntervalForDays(days int) (interval string, intervalCount int) {
	if days <= 0 {
		return "month", 1
	}
	switch days {
	case 7:
		return "week", 1
	case 30:
		return "month", 1
	case 365:
		return "year", 1
	default:
		// Stripe supports day interval with interval_count.
		return "day", days
	}
}

func stripePostForm(ctx context.Context, secretKey string, endpoint string, form url.Values, idempotencyKey string) (*stripeObject, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if strings.TrimSpace(idempotencyKey) != "" {
		req.Header.Set("Idempotency-Key", strings.TrimSpace(idempotencyKey))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("stripe error: %s", parseStripeError(body))
	}
	var out stripeObject
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.ID) == "" {
		return nil, fmt.Errorf("stripe response missing id")
	}
	return &out, nil
}

func parseStripeError(body []byte) string {
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.Error.Message)
}
