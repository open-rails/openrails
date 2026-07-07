package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/railresolve"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
)

// RequireStripeSecretKey resolves the ctx merchant's armed Stripe account —
// the rail_merchant_accounts row plus scoped secrets (Layer C, #788). It never
// reads a boot-config artifact; an unarmed rail fails closed.
func RequireStripeSecretKey(ctx context.Context, src railresolve.Source) (*config.RailMerchantAccountConfig, string, error) {
	if src == nil {
		return nil, "", fmt.Errorf("stripe configuration is not available")
	}
	proc, err := src.RailConfig(ctx, string(models.RailStripe), "")
	if err != nil {
		return nil, "", fmt.Errorf("stripe configuration is not available: %w", err)
	}
	if proc.Stripe == nil {
		return nil, "", fmt.Errorf("stripe configuration is not available")
	}
	secretKey := strings.TrimSpace(proc.Stripe.SecretKey)
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
	Rails  railresolve.Source

	// baseURL overrides the Stripe API root. Empty means the production Stripe
	// API (https://api.stripe.com). Tests set this to an httptest server.
	baseURL string
}

// stripeBaseURL returns the Stripe API root, honoring the test override.
func (s *StripeService) stripeBaseURL() string {
	if s != nil && strings.TrimSpace(s.baseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(s.baseURL), "/")
	}
	return "https://api.stripe.com"
}

// CreateCustomer creates a Stripe Customer for the given app user, tagging it
// with metadata[app_user_id] so it can be re-discovered later. The request is
// idempotent on the app user id, so retries (or two parallel checkouts) cannot
// mint duplicate customers.
func (s *StripeService) CreateCustomer(ctx context.Context, email, appUserID string) (string, error) {
	_, secretKey, err := RequireStripeSecretKey(ctx, s.Rails)
	if err != nil {
		return "", err
	}
	appUserID = strings.TrimSpace(appUserID)
	if appUserID == "" {
		return "", errors.New("app_user_id is required")
	}
	values := url.Values{}
	if email = strings.TrimSpace(email); email != "" {
		values.Set("email", email)
	}
	values.Set("metadata[app_user_id]", appUserID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.stripeBaseURL()+"/v1/customers", strings.NewReader(values.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Idempotency on the app user id prevents duplicate customers across retries
	// and concurrent checkouts.
	req.Header.Set("Idempotency-Key", "customer_create_"+appUserID)

	client := stripeapi.Client(s.Config, 0)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("stripe customer create failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read stripe customer create response: %w", err)
	}
	if resp.StatusCode >= 400 {
		msg := ParseStripeAPIError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe customer create failed (%d)", resp.StatusCode)
		}
		return "", errors.New(msg)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("stripe customer create parse failed: %w", err)
	}
	if strings.TrimSpace(out.ID) == "" {
		return "", errors.New("stripe customer create returned empty id")
	}
	return out.ID, nil
}

// FindCustomerIDByAppUserID looks up an existing Stripe Customer by the
// metadata[app_user_id] tag using Stripe Customer Search. It returns "" when no
// match exists (not an error) so callers can fall back to creating one.
func (s *StripeService) FindCustomerIDByAppUserID(ctx context.Context, appUserID string) (string, error) {
	_, secretKey, err := RequireStripeSecretKey(ctx, s.Rails)
	if err != nil {
		return "", err
	}
	appUserID = strings.TrimSpace(appUserID)
	if appUserID == "" {
		return "", errors.New("app_user_id is required")
	}
	query := url.Values{}
	query.Set("query", fmt.Sprintf("metadata['app_user_id']:'%s'", appUserID))
	query.Set("limit", "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.stripeBaseURL()+"/v1/customers/search?"+query.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)

	client := stripeapi.Client(s.Config, 0)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("stripe customer search failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read stripe customer search response: %w", err)
	}
	if resp.StatusCode >= 400 {
		msg := ParseStripeAPIError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe customer search failed (%d)", resp.StatusCode)
		}
		return "", errors.New(msg)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("stripe customer search parse failed: %w", err)
	}
	if len(out.Data) == 0 {
		return "", nil
	}
	return strings.TrimSpace(out.Data[0].ID), nil
}

// StripeSubscriptionSummary is a minimal view of a Stripe subscription used by
// the webhook-independent duplicate guard.
type StripeSubscriptionSummary struct {
	ID        string
	Status    string
	PriceID   string
	LookupKey string
}

// ListActiveSubscriptionsForCustomer returns the customer's active and trialing
// Stripe subscriptions. It queries both statuses because a subscription in a
// trial still represents a committed plan for tier-group purposes.
func (s *StripeService) ListActiveSubscriptionsForCustomer(ctx context.Context, customerID string) ([]StripeSubscriptionSummary, error) {
	_, secretKey, err := RequireStripeSecretKey(ctx, s.Rails)
	if err != nil {
		return nil, err
	}
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return nil, errors.New("customer_id is required")
	}

	client := stripeapi.Client(s.Config, 0)
	var summaries []StripeSubscriptionSummary
	for _, status := range []string{"active", "trialing"} {
		query := url.Values{}
		query.Set("customer", customerID)
		query.Set("status", status)
		query.Set("limit", "100")

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.stripeBaseURL()+"/v1/subscriptions?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+secretKey)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("stripe subscription list failed: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read stripe subscription list response: %w", err)
		}
		if resp.StatusCode >= 400 {
			msg := ParseStripeAPIError(body)
			if msg == "" {
				msg = fmt.Sprintf("stripe subscription list failed (%d)", resp.StatusCode)
			}
			return nil, errors.New(msg)
		}
		var out struct {
			Data []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Items  struct {
					Data []struct {
						Price struct {
							ID        string `json:"id"`
							LookupKey string `json:"lookup_key"`
						} `json:"price"`
					} `json:"data"`
				} `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("stripe subscription list parse failed: %w", err)
		}
		for _, sub := range out.Data {
			summary := StripeSubscriptionSummary{ID: sub.ID, Status: sub.Status}
			if len(sub.Items.Data) > 0 {
				summary.PriceID = strings.TrimSpace(sub.Items.Data[0].Price.ID)
				summary.LookupKey = strings.TrimSpace(sub.Items.Data[0].Price.LookupKey)
			}
			summaries = append(summaries, summary)
		}
	}
	return summaries, nil
}

// SetBaseURLForTest overrides the Stripe API root. Test-only.
func (s *StripeService) SetBaseURLForTest(baseURL string) {
	if s != nil {
		s.baseURL = baseURL
	}
}

func (s *StripeService) GetSubscriptionItemID(ctx context.Context, subscriptionID string) (string, error) {
	_, secretKey, err := RequireStripeSecretKey(ctx, s.Rails)
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

	client := stripeapi.Client(s.Config, 0)
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

// UpdateSubscriptionPrice swaps the subscription's line-item price and, when
// internalPriceID is non-empty, rewrites the subscription's
// metadata[internal_price_id] to the new local price UUID. The metadata rewrite
// is essential: every invoice Stripe emits for this subscription (the immediate
// proration invoice AND all future renewals) carries the subscription's metadata
// under subscription_details.metadata, and the invoice.paid webhook resolves the
// price from internal_price_id first. If we changed only the line-item price but
// left stale metadata pointing at the OLD price, the upgrade proration invoice
// and every later renewal would resolve the old price, fail the amount check,
// and be dropped (#268).
func (s *StripeService) UpdateSubscriptionPrice(ctx context.Context, subscriptionID, itemID, newPriceID, internalPriceID, prorationBehavior, billingAnchor string) error {
	_, secretKey, err := RequireStripeSecretKey(ctx, s.Rails)
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
	if internalPriceID = strings.TrimSpace(internalPriceID); internalPriceID != "" {
		values.Set("metadata[internal_price_id]", internalPriceID)
	}
	if strings.TrimSpace(prorationBehavior) != "" {
		values.Set("proration_behavior", prorationBehavior)
	}
	if strings.TrimSpace(billingAnchor) != "" {
		values.Set("billing_cycle_anchor", billingAnchor)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.stripeBaseURL()+"/v1/subscriptions/"+subscriptionID, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := stripeapi.Client(s.Config, 0)
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
	_, secretKey, err := RequireStripeSecretKey(ctx, s.Rails)
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

	client := stripeapi.Client(s.Config, 0)
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
		interval, intervalCount = sharedformat.BillingCycleDaysToStripeRecurring(*billingCycleDays)
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
	_, secretKey, err := RequireStripeSecretKey(ctx, s.Rails)
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

	client := stripeapi.Client(s.Config, 0)
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
	_, secretKey, err := RequireStripeSecretKey(ctx, s.Rails)
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

	client := stripeapi.Client(s.Config, 0)
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
