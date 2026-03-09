package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
)

type StripePortalService struct {
	Config *config.Config
}

func (s *StripePortalService) CreatePortalSession(ctx context.Context, customerID, returnURL string) (string, error) {
	_, secretKey, err := requireStripeSecretKey(s.Config)
	if err != nil {
		return "", err
	}
	customerID = strings.TrimSpace(customerID)
	returnURL = strings.TrimSpace(returnURL)
	if customerID == "" || returnURL == "" {
		return "", errors.New("customer_id and return_url are required")
	}

	values := url.Values{}
	values.Set("customer", customerID)
	values.Set("return_url", returnURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.stripe.com/v1/billing_portal/sessions", strings.NewReader(values.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("stripe portal failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		msg := parseStripePortalError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe portal failed (%d)", resp.StatusCode)
		}
		return "", errors.New(msg)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("stripe portal parse failed: %w", err)
	}
	if strings.TrimSpace(out.URL) == "" {
		return "", errors.New("stripe portal returned empty URL")
	}
	return out.URL, nil
}

func parseStripePortalError(body []byte) string {
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
