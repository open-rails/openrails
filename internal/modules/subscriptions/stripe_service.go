package subscriptions

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

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/railresolve"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
)

// RequireStripeSecretKey resolves the ctx merchant's armed Stripe account (the
// psps row plus scoped secrets). An unarmed rail fails closed.
func RequireStripeSecretKey(ctx context.Context, src railresolve.Source) (*config.ResolvedPSP, string, error) {
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
	StripeClients     *stripeapi.Factory
	accountMerchantID uuid.UUID
	accountPSPID      uuid.UUID
	accountID         string
	accountSecret     string
	Config            *config.Config
	Rails             railresolve.Source
}

// CreateCustomer creates a Stripe Customer for the given app user, tagging it
// with metadata[app_user_id] so it can be re-discovered later. The request is
// idempotent on the app user id, so retries (or two parallel purchases) cannot
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stripeapi.APIBase+"/v1/customers", strings.NewReader(values.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Idempotency on the app user id prevents duplicate customers across retries
	// and concurrent purchases.
	req.Header.Set("Idempotency-Key", "customer_create_"+appUserID)

	client := s.StripeClients.Client(0)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stripeapi.APIBase+"/v1/customers/search?"+query.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)

	client := s.StripeClients.Client(0)
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

	client := s.StripeClients.Client(0)
	var summaries []StripeSubscriptionSummary
	for _, status := range []string{"active", "trialing"} {
		query := url.Values{}
		query.Set("customer", customerID)
		query.Set("status", status)
		query.Set("limit", "100")

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, stripeapi.APIBase+"/v1/subscriptions?"+query.Encode(), nil)
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

	client := s.StripeClients.Client(0)
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
// internalPriceID is non-empty, rewrites metadata[internal_price_id] to it.
// Every invoice Stripe emits (proration and renewals) carries that metadata,
// and invoice.paid resolves the price from it first: stale metadata would fail
// the amount check and drop those invoices.
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stripeapi.APIBase+"/v1/subscriptions/"+subscriptionID, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := s.StripeClients.Client(0)
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

// ScheduleSubscriptionPriceChange plants a schedule that keeps the current
// price and seats until the current period ends, then bills newPriceID for
// the same seats and releases. key (the move's id) makes both writes
// idempotent: a retry after a lost answer replays them instead of finding the
// schedule its first attempt attached.
func (s *StripeService) ScheduleSubscriptionPriceChange(ctx context.Context, key, subscriptionID, currentPriceID, newPriceID string, currentPeriodStart, currentPeriodEnd time.Time, billingCycleDays *int) error {
	subscriptionID = strings.TrimSpace(subscriptionID)
	currentPriceID = strings.TrimSpace(currentPriceID)
	newPriceID = strings.TrimSpace(newPriceID)
	if subscriptionID == "" || currentPriceID == "" || newPriceID == "" {
		return errors.New("subscription_id, current_price_id, and new_price_id are required")
	}
	if currentPeriodEnd.IsZero() {
		return errors.New("current_period_end is required to schedule a Stripe price change")
	}
	created, err := s.CreateScheduleFromSubscription(ctx, subscriptionID, key)
	if err != nil {
		return fmt.Errorf("stripe subscription schedule create failed: %w", err)
	}
	current := StripeSchedulePhase{PriceID: currentPriceID, StartDate: currentPeriodStart.Unix(), EndDate: currentPeriodEnd.Unix(), Quantity: 1}
	if len(created.Phases) > 0 {
		first := created.Phases[0]
		if first.StartDate > 0 {
			current.StartDate = first.StartDate
		}
		if first.EndDate > 0 {
			current.EndDate = first.EndDate
		}
		if first.PriceID != "" {
			current.PriceID = first.PriceID
		}
		if first.Quantity > 0 {
			current.Quantity = first.Quantity
		}
	}
	if current.StartDate <= 0 || current.EndDate <= current.StartDate {
		return fmt.Errorf("stripe subscription schedule %s has no open current phase", created.ID)
	}
	if _, err := s.stripePostForm(ctx, "/v1/subscription_schedules/"+url.PathEscape(created.ID), schedulePhasesForm(current, newPriceID, billingCycleDays), key+":phases"); err != nil {
		return fmt.Errorf("stripe subscription schedule update failed: %w", err)
	}
	return nil
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

	client := s.StripeClients.Client(0)
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

// EndSubscription cancels a Stripe subscription now (DELETE), stopping any
// retry of its open invoice. A subscription Stripe no longer has is already
// ended.
func (s *StripeService) EndSubscription(ctx context.Context, subscriptionID string) error {
	_, secretKey, err := RequireStripeSecretKey(ctx, s.Rails)
	if err != nil {
		return err
	}
	subscriptionID = strings.TrimSpace(subscriptionID)
	if subscriptionID == "" {
		return errors.New("subscription_id is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "https://api.stripe.com/v1/subscriptions/"+url.PathEscape(subscriptionID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	resp, err := s.StripeClients.Client(0).Do(req)
	if err != nil {
		return fmt.Errorf("stripe subscription end failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read stripe subscription end response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 400 {
		msg := ParseStripeAPIError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe subscription end failed (%d)", resp.StatusCode)
		}
		return errors.New(msg)
	}
	return nil
}

// NewAccountStripeService binds a resolved account and its credentials together.
// Receipt reads use the captured credentials, even if the general Rails view is
// changed by a host after construction.
func NewAccountStripeService(cfg *config.Config, merchantID, pspID uuid.UUID, accountID, secretKey string) *StripeService {
	return &StripeService{Config: cfg, accountMerchantID: merchantID, accountPSPID: pspID, accountID: accountID, accountSecret: secretKey, Rails: railresolve.FixedSet{"stripe": {Rail: models.RailStripe, AccountID: accountID, Stripe: &config.StripeRailConfig{SecretKey: secretKey}}}}
}
func (s *StripeService) AccountIdentity() (uuid.UUID, uuid.UUID) {
	return s.accountMerchantID, s.accountPSPID
}

// keyLive reports whether the scoped account key is live: what Stripe's
// livemode must say of every object it returns.
func (s *StripeService) keyLive() bool {
	return strings.HasPrefix(s.accountSecret, "sk_live_") || strings.HasPrefix(s.accountSecret, "rk_live_")
}
