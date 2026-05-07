package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
)

func requireStripeProcessorConfig(cfg *config.Config) (*config.ProcessorConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("stripe configuration is not available")
	}
	proc := cfg.GetStripeProcessor()
	if proc == nil {
		return nil, fmt.Errorf("stripe configuration is not available")
	}
	return proc, nil
}

func RequireStripeSecretKey(cfg *config.Config) (*config.ProcessorConfig, string, error) {
	proc, err := requireStripeProcessorConfig(cfg)
	if err != nil {
		return nil, "", err
	}
	secretKey := strings.TrimSpace(proc.SecretKey)
	if secretKey == "" {
		return nil, "", fmt.Errorf("stripe secret key is not configured")
	}
	return proc, secretKey, nil
}

func ParseStripeAPIError(body []byte) string {
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

type StripeService struct {
	Config *config.Config
}

func (s *StripeService) GetSubscriptionItemID(ctx context.Context, subscriptionID string) (string, error) {
	_, secretKey, err := RequireStripeSecretKey(s.Config)
	if err != nil {
		return "", err
	}
	subscriptionID = strings.TrimSpace(subscriptionID)
	if subscriptionID == "" {
		return "", errors.New("subscription_id is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.stripe.com/v1/subscriptions/"+subscriptionID, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("stripe subscription fetch failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read stripe subscription response: %w", err)
	}
	if resp.StatusCode >= 400 {
		msg := ParseStripeAPIError(body)
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

func (s *StripeService) UpdateSubscriptionPrice(ctx context.Context, subscriptionID, itemID, newPriceID, prorationBehavior, billingAnchor string) error {
	_, secretKey, err := RequireStripeSecretKey(s.Config)
	if err != nil {
		return err
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
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stripe subscription update failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read stripe subscription update response: %w", err)
	}
	if resp.StatusCode >= 400 {
		msg := ParseStripeAPIError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe subscription update failed (%d)", resp.StatusCode)
		}
		return errors.New(msg)
	}
	return nil
}

type stripeSubscriptionScheduleResponse struct {
	ID     string `json:"id"`
	Phases []struct {
		StartDate int64 `json:"start_date"`
		EndDate   int64 `json:"end_date"`
		Items     []struct {
			Price    string `json:"price"`
			Quantity int64  `json:"quantity"`
		} `json:"items"`
	} `json:"phases"`
}

func (s *StripeService) ScheduleSubscriptionPriceChange(ctx context.Context, subscriptionID, currentPriceID, newPriceID string, currentPeriodStart, currentPeriodEnd time.Time, billingCycleDays *int) (string, error) {
	_, secretKey, err := RequireStripeSecretKey(s.Config)
	if err != nil {
		return "", err
	}
	subscriptionID = strings.TrimSpace(subscriptionID)
	currentPriceID = strings.TrimSpace(currentPriceID)
	newPriceID = strings.TrimSpace(newPriceID)
	if subscriptionID == "" || currentPriceID == "" || newPriceID == "" {
		return "", errors.New("subscription_id, current_price_id, and new_price_id are required")
	}
	if currentPeriodEnd.IsZero() {
		return "", errors.New("current_period_end is required to schedule a Stripe price change")
	}

	createValues := url.Values{}
	createValues.Set("from_subscription", subscriptionID)
	createReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.stripe.com/v1/subscription_schedules", strings.NewReader(createValues.Encode()))
	if err != nil {
		return "", err
	}
	createReq.Header.Set("Authorization", "Bearer "+secretKey)
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 20 * time.Second}
	createResp, err := client.Do(createReq)
	if err != nil {
		return "", fmt.Errorf("stripe subscription schedule create failed: %w", err)
	}
	defer createResp.Body.Close()
	createBody, err := io.ReadAll(createResp.Body)
	if err != nil {
		return "", fmt.Errorf("read stripe subscription schedule create response: %w", err)
	}
	if createResp.StatusCode >= 400 {
		msg := ParseStripeAPIError(createBody)
		if msg == "" {
			msg = fmt.Sprintf("stripe subscription schedule create failed (%d)", createResp.StatusCode)
		}
		return "", errors.New(msg)
	}

	var schedule stripeSubscriptionScheduleResponse
	if err := json.Unmarshal(createBody, &schedule); err != nil {
		return "", fmt.Errorf("stripe subscription schedule parse failed: %w", err)
	}
	if strings.TrimSpace(schedule.ID) == "" {
		return "", errors.New("stripe subscription schedule id missing")
	}

	startDate := currentPeriodStart.Unix()
	endDate := currentPeriodEnd.Unix()
	quantity := int64(1)
	if len(schedule.Phases) > 0 {
		if schedule.Phases[0].StartDate > 0 {
			startDate = schedule.Phases[0].StartDate
		}
		if schedule.Phases[0].EndDate > 0 {
			endDate = schedule.Phases[0].EndDate
		}
		if len(schedule.Phases[0].Items) > 0 {
			if strings.TrimSpace(schedule.Phases[0].Items[0].Price) != "" {
				currentPriceID = schedule.Phases[0].Items[0].Price
			}
			if schedule.Phases[0].Items[0].Quantity > 0 {
				quantity = schedule.Phases[0].Items[0].Quantity
			}
		}
	}
	if startDate <= 0 {
		startDate = time.Now().UTC().Unix()
	}

	interval := "month"
	intervalCount := 1
	if billingCycleDays != nil && *billingCycleDays > 0 {
		interval, intervalCount = sharedformat.BillingCycleDaysToInterval(*billingCycleDays)
	}

	updateValues := url.Values{}
	updateValues.Set("end_behavior", "release")
	updateValues.Set("proration_behavior", "none")
	updateValues.Set("phases[0][items][0][price]", currentPriceID)
	updateValues.Set("phases[0][items][0][quantity]", strconv.FormatInt(quantity, 10))
	updateValues.Set("phases[0][start_date]", strconv.FormatInt(startDate, 10))
	updateValues.Set("phases[0][end_date]", strconv.FormatInt(endDate, 10))
	updateValues.Set("phases[1][items][0][price]", newPriceID)
	updateValues.Set("phases[1][items][0][quantity]", "1")
	updateValues.Set("phases[1][duration][interval]", interval)
	updateValues.Set("phases[1][duration][interval_count]", strconv.Itoa(intervalCount))

	updateReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.stripe.com/v1/subscription_schedules/"+schedule.ID, strings.NewReader(updateValues.Encode()))
	if err != nil {
		return "", err
	}
	updateReq.Header.Set("Authorization", "Bearer "+secretKey)
	updateReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	updateResp, err := client.Do(updateReq)
	if err != nil {
		return "", fmt.Errorf("stripe subscription schedule update failed: %w", err)
	}
	defer updateResp.Body.Close()
	updateBody, err := io.ReadAll(updateResp.Body)
	if err != nil {
		return "", fmt.Errorf("read stripe subscription schedule update response: %w", err)
	}
	if updateResp.StatusCode >= 400 {
		msg := ParseStripeAPIError(updateBody)
		if msg == "" {
			msg = fmt.Sprintf("stripe subscription schedule update failed (%d)", updateResp.StatusCode)
		}
		return "", errors.New(msg)
	}

	return schedule.ID, nil
}

func (s *StripeService) CancelSubscription(ctx context.Context, subscriptionID string) error {
	_, secretKey, err := RequireStripeSecretKey(s.Config)
	if err != nil {
		return err
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
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stripe subscription cancel failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read stripe subscription cancel response: %w", err)
	}
	if resp.StatusCode >= 400 {
		msg := ParseStripeAPIError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe subscription cancel failed (%d)", resp.StatusCode)
		}
		return errors.New(msg)
	}
	return nil
}

func (s *StripeService) ResumeSubscription(ctx context.Context, subscriptionID string) error {
	_, secretKey, err := RequireStripeSecretKey(s.Config)
	if err != nil {
		return err
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
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stripe subscription resume failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read stripe subscription resume response: %w", err)
	}
	if resp.StatusCode >= 400 {
		msg := ParseStripeAPIError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe subscription resume failed (%d)", resp.StatusCode)
		}
		return errors.New(msg)
	}
	return nil
}
