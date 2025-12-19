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

	"github.com/doujins-org/doujins-billing/config"
)

type StripeSubscriptionService struct {
	Config *config.Config
}

func (s *StripeSubscriptionService) GetSubscriptionItemID(ctx context.Context, subscriptionID string) (string, error) {
	if s == nil || s.Config == nil || s.Config.Stripe == nil {
		return "", errors.New("stripe configuration is not available")
	}
	if strings.TrimSpace(s.Config.Stripe.SecretKey) == "" {
		return "", errors.New("stripe secret key is not configured")
	}
	subscriptionID = strings.TrimSpace(subscriptionID)
	if subscriptionID == "" {
		return "", errors.New("subscription_id is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.stripe.com/v1/subscriptions/"+subscriptionID, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.Config.Stripe.SecretKey))

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("stripe subscription fetch failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		msg := parseStripePortalError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe subscription fetch failed (%d)", resp.StatusCode)
		}
		return "", errors.New(msg)
	}
	var out struct {
		Items struct {
			Data []struct {
				ID    string `json:"id"`
				Price struct {
					ID string `json:"id"`
				} `json:"price"`
			} `json:"data"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("stripe subscription parse failed: %w", err)
	}
	if len(out.Items.Data) == 0 || strings.TrimSpace(out.Items.Data[0].ID) == "" {
		return "", errors.New("stripe subscription item not found")
	}
	return out.Items.Data[0].ID, nil
}

func (s *StripeSubscriptionService) UpdateSubscriptionPrice(ctx context.Context, subscriptionID, itemID, newPriceID, prorationBehavior, billingAnchor string) error {
	if s == nil || s.Config == nil || s.Config.Stripe == nil {
		return errors.New("stripe configuration is not available")
	}
	if strings.TrimSpace(s.Config.Stripe.SecretKey) == "" {
		return errors.New("stripe secret key is not configured")
	}
	subscriptionID = strings.TrimSpace(subscriptionID)
	itemID = strings.TrimSpace(itemID)
	newPriceID = strings.TrimSpace(newPriceID)
	if subscriptionID == "" || itemID == "" || newPriceID == "" {
		return errors.New("subscription_id, item_id, and price_id are required")
	}
	values := url.Values{}
	values.Set("items[0][id]", itemID)
	values.Set("items[0][price]", newPriceID)
	if strings.TrimSpace(prorationBehavior) != "" {
		values.Set("proration_behavior", prorationBehavior)
	}
	if strings.TrimSpace(billingAnchor) != "" {
		values.Set("billing_cycle_anchor", billingAnchor)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.stripe.com/v1/subscriptions/"+subscriptionID, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.Config.Stripe.SecretKey))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stripe subscription update failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		msg := parseStripePortalError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe subscription update failed (%d)", resp.StatusCode)
		}
		return errors.New(msg)
	}
	return nil
}

func (s *StripeSubscriptionService) CancelSubscription(ctx context.Context, subscriptionID string) error {
	if s == nil || s.Config == nil || s.Config.Stripe == nil {
		return errors.New("stripe configuration is not available")
	}
	if strings.TrimSpace(s.Config.Stripe.SecretKey) == "" {
		return errors.New("stripe secret key is not configured")
	}
	subscriptionID = strings.TrimSpace(subscriptionID)
	if subscriptionID == "" {
		return errors.New("subscription_id is required")
	}
	values := url.Values{}
	values.Set("cancel_at_period_end", "true")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.stripe.com/v1/subscriptions/"+subscriptionID, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.Config.Stripe.SecretKey))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stripe subscription cancel failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		msg := parseStripePortalError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe subscription cancel failed (%d)", resp.StatusCode)
		}
		return errors.New(msg)
	}
	return nil
}

func (s *StripeSubscriptionService) ResumeSubscription(ctx context.Context, subscriptionID string) error {
	if s == nil || s.Config == nil || s.Config.Stripe == nil {
		return errors.New("stripe configuration is not available")
	}
	if strings.TrimSpace(s.Config.Stripe.SecretKey) == "" {
		return errors.New("stripe secret key is not configured")
	}
	subscriptionID = strings.TrimSpace(subscriptionID)
	if subscriptionID == "" {
		return errors.New("subscription_id is required")
	}
	values := url.Values{}
	values.Set("cancel_at_period_end", "false")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.stripe.com/v1/subscriptions/"+subscriptionID, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.Config.Stripe.SecretKey))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stripe subscription resume failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		msg := parseStripePortalError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe subscription resume failed (%d)", resp.StatusCode)
		}
		return errors.New(msg)
	}
	return nil
}
