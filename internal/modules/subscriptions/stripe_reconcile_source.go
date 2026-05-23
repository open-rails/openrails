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
)

// StripeRemoteSubscription is a normalized view of a Stripe subscription as
// returned by the Stripe REST API, carrying only the fields the reconcile
// command needs to map it back to a local membership.
type StripeRemoteSubscription struct {
	ID                 string
	Status             string
	CustomerID         string
	StripePriceID      string
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	CancelAtPeriodEnd  bool
	Metadata           map[string]string
}

// StripeSubscriptionLister pages through remote Stripe subscriptions. It is an
// interface so the reconcile command can be unit tested without hitting the
// live Stripe API.
type StripeSubscriptionLister interface {
	ListSubscriptions(ctx context.Context, pageSize, maxPages int) ([]StripeRemoteSubscription, error)
}

// HTTPStripeSubscriptionLister is the production lister that talks to the
// Stripe REST API using the configured secret key.
type HTTPStripeSubscriptionLister struct {
	SecretKey  string
	HTTPClient *http.Client
}

// NewStripeSubscriptionLister builds a lister from the OpenRails config,
// reusing RequireStripeSecretKey for auth.
func NewStripeSubscriptionLister(cfg *config.Config) (*HTTPStripeSubscriptionLister, error) {
	_, secretKey, err := RequireStripeSecretKey(cfg)
	if err != nil {
		return nil, err
	}
	return &HTTPStripeSubscriptionLister{SecretKey: secretKey}, nil
}

type stripeSubscriptionListResponse struct {
	HasMore bool                         `json:"has_more"`
	Data    []stripeSubscriptionEnvelope `json:"data"`
}

type stripeSubscriptionEnvelope struct {
	ID                 string            `json:"id"`
	Status             string            `json:"status"`
	Customer           string            `json:"customer"`
	Metadata           map[string]string `json:"metadata"`
	CancelAtPeriodEnd  bool              `json:"cancel_at_period_end"`
	CurrentPeriodStart int64             `json:"current_period_start"`
	CurrentPeriodEnd   int64             `json:"current_period_end"`
	Items              struct {
		Data []struct {
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

// parseStripeSubscriptionList decodes a single Stripe subscriptions list page
// into normalized records. It is split out from the HTTP path so the parsing /
// price-item extraction can be unit tested against fixtures.
func parseStripeSubscriptionList(body []byte) ([]StripeRemoteSubscription, bool, error) {
	var resp stripeSubscriptionListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false, fmt.Errorf("stripe subscription list parse failed: %w", err)
	}
	out := make([]StripeRemoteSubscription, 0, len(resp.Data))
	for _, env := range resp.Data {
		sub := StripeRemoteSubscription{
			ID:         strings.TrimSpace(env.ID),
			Status:     strings.TrimSpace(env.Status),
			CustomerID: strings.TrimSpace(env.Customer),
			Metadata:   env.Metadata,
		}
		if len(env.Items.Data) > 0 {
			sub.StripePriceID = strings.TrimSpace(env.Items.Data[0].Price.ID)
		}
		if env.CurrentPeriodStart > 0 {
			sub.CurrentPeriodStart = time.Unix(env.CurrentPeriodStart, 0).UTC()
		}
		if env.CurrentPeriodEnd > 0 {
			sub.CurrentPeriodEnd = time.Unix(env.CurrentPeriodEnd, 0).UTC()
		}
		sub.CancelAtPeriodEnd = env.CancelAtPeriodEnd
		out = append(out, sub)
	}
	return out, resp.HasMore, nil
}

// ListSubscriptions pages through GET /v1/subscriptions?status=all using cursor
// pagination (starting_after). pageSize bounds the limit per request (Stripe
// caps it at 100); maxPages bounds the number of pages fetched (0 = unlimited).
func (l *HTTPStripeSubscriptionLister) ListSubscriptions(ctx context.Context, pageSize, maxPages int) ([]StripeRemoteSubscription, error) {
	if strings.TrimSpace(l.SecretKey) == "" {
		return nil, errors.New("stripe secret key is required")
	}
	if pageSize <= 0 {
		pageSize = 100
	}
	if pageSize > 100 {
		pageSize = 100
	}

	client := l.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	var (
		all           []StripeRemoteSubscription
		startingAfter string
	)

	for page := 0; ; page++ {
		if maxPages > 0 && page >= maxPages {
			break
		}

		values := url.Values{}
		values.Set("status", "all")
		values.Set("limit", strconv.Itoa(pageSize))
		// Expand the line-item price so we can read the Stripe price id.
		values.Set("expand[]", "data.items.data.price")
		if startingAfter != "" {
			values.Set("starting_after", startingAfter)
		}

		reqURL := "https://api.stripe.com/v1/subscriptions?" + values.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+l.SecretKey)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("stripe subscription list failed: %w", err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read stripe subscription list response: %w", readErr)
		}
		if resp.StatusCode >= 400 {
			msg := ParseStripeAPIError(body)
			if msg == "" {
				msg = fmt.Sprintf("stripe subscription list failed (%d)", resp.StatusCode)
			}
			return nil, errors.New(msg)
		}

		subs, hasMore, err := parseStripeSubscriptionList(body)
		if err != nil {
			return nil, err
		}
		all = append(all, subs...)

		if !hasMore || len(subs) == 0 {
			break
		}
		startingAfter = subs[len(subs)-1].ID
		if startingAfter == "" {
			break
		}
	}

	return all, nil
}

// StripeSubscriptionStatusActive reports whether a Stripe subscription status
// should be treated as an active membership for reconciliation purposes.
func StripeSubscriptionStatusActive(status string) bool {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "active", "trialing":
		return true
	default:
		return false
	}
}
