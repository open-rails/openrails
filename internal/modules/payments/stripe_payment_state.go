package payments

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

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
)

const stripeRESTBaseURL = "https://api.stripe.com"

// ErrStripeObjectNotFound reports provider truth that no longer has the object.
var ErrStripeObjectNotFound = errors.New("stripe object not found")

// StripePaymentMethodState is fetched Stripe truth for one stored card.
type StripePaymentMethodState struct {
	ID         string
	CustomerID string
	Card       *StripeCard
}

// StripeSubscriptionPaymentState is the effective card selection for one
// Stripe subscription after applying Stripe's subscription-over-customer
// default precedence.
type StripeSubscriptionPaymentState struct {
	SubscriptionID string
	PaymentMethod  *StripePaymentMethodState
}

// StripeCustomerPaymentState contains effective defaults for one customer.
type StripeCustomerPaymentState struct {
	CustomerID    string
	Subscriptions []StripeSubscriptionPaymentState
}

// StripePaymentStateReader fetches current state from one exact Stripe account.
type StripePaymentStateReader interface {
	PaymentMethod(ctx context.Context, paymentMethodID string) (*StripePaymentMethodState, error)
	CustomerPaymentState(ctx context.Context, customerID string) (*StripeCustomerPaymentState, error)
}

// HTTPStripePaymentStateReader reads Stripe state through the read-only Stripe
// transport. BaseURL and HTTPClient are test seams; production leaves both
// empty and uses the pinned Stripe API transport.
type HTTPStripePaymentStateReader struct {
	SecretKey  string
	BaseURL    string
	HTTPClient *http.Client
	PageLimit  int
}

func NewHTTPStripePaymentStateReader(secretKey string) *HTTPStripePaymentStateReader {
	return &HTTPStripePaymentStateReader{SecretKey: strings.TrimSpace(secretKey)}
}

func (r *HTTPStripePaymentStateReader) PaymentMethod(ctx context.Context, paymentMethodID string) (*StripePaymentMethodState, error) {
	paymentMethodID = strings.TrimSpace(paymentMethodID)
	if paymentMethodID == "" {
		return nil, nil
	}
	body, err := r.get(ctx, "/v1/payment_methods/"+url.PathEscape(paymentMethodID))
	if err != nil {
		if errors.Is(err, ErrStripeObjectNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("retrieve stripe payment method: %w", err)
	}
	state, err := parseStripePaymentMethodState(body)
	if err != nil {
		return nil, fmt.Errorf("parse stripe payment method: %w", err)
	}
	return state, nil
}

func (r *HTTPStripePaymentStateReader) CustomerPaymentState(ctx context.Context, customerID string) (*StripeCustomerPaymentState, error) {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return nil, nil
	}

	customerBody, err := r.get(ctx, "/v1/customers/"+url.PathEscape(customerID)+"?expand[]=invoice_settings.default_payment_method")
	if err != nil {
		return nil, fmt.Errorf("retrieve stripe customer: %w", err)
	}
	var customer struct {
		ID              string `json:"id"`
		Deleted         bool   `json:"deleted"`
		InvoiceSettings struct {
			DefaultPaymentMethod json.RawMessage `json:"default_payment_method"`
		} `json:"invoice_settings"`
	}
	if err := json.Unmarshal(customerBody, &customer); err != nil {
		return nil, fmt.Errorf("parse stripe customer: %w", err)
	}
	if customer.Deleted {
		return nil, ErrStripeObjectNotFound
	}
	returnedCustomerID := strings.TrimSpace(customer.ID)
	if returnedCustomerID == "" {
		return nil, errors.New("stripe customer response missing id")
	}
	if returnedCustomerID != customerID {
		return nil, fmt.Errorf("stripe customer response id %q does not match requested customer %q", returnedCustomerID, customerID)
	}

	defaultID := stripeObjectID(customer.InvoiceSettings.DefaultPaymentMethod)
	remoteSubscriptions, err := r.listSubscriptions(ctx, customerID)
	if err != nil {
		return nil, err
	}

	methodCache := make(map[string]*StripePaymentMethodState)
	loadMethod := func(methodID string) (*StripePaymentMethodState, error) {
		methodID = strings.TrimSpace(methodID)
		if methodID == "" {
			return nil, nil
		}
		if cached, ok := methodCache[methodID]; ok {
			return cached, nil
		}
		method, err := r.PaymentMethod(ctx, methodID)
		if err != nil {
			return nil, err
		}
		methodCache[methodID] = method
		return method, nil
	}

	state := &StripeCustomerPaymentState{CustomerID: returnedCustomerID}
	for _, remote := range remoteSubscriptions {
		methodID := remote.DefaultPaymentMethodID
		if methodID == "" {
			methodID = defaultID
		}
		method, err := loadMethod(methodID)
		if err != nil {
			return nil, fmt.Errorf("load stripe subscription %s payment method: %w", remote.ID, err)
		}
		if method != nil && strings.TrimSpace(method.CustomerID) != customerID {
			method = nil
		}
		state.Subscriptions = append(state.Subscriptions, StripeSubscriptionPaymentState{
			SubscriptionID: remote.ID,
			PaymentMethod:  method,
		})
	}
	return state, nil
}

type stripeSubscriptionPaymentJSON struct {
	ID                      string          `json:"id"`
	DefaultPaymentMethodRaw json.RawMessage `json:"default_payment_method"`
}

type stripeSubscriptionPayment struct {
	ID                     string
	DefaultPaymentMethodID string
}

func (r *HTTPStripePaymentStateReader) listSubscriptions(ctx context.Context, customerID string) ([]stripeSubscriptionPayment, error) {
	var (
		out           []stripeSubscriptionPayment
		startingAfter string
	)
	for {
		query := url.Values{}
		query.Set("customer", customerID)
		query.Set("status", "all")
		query.Set("limit", strconv.Itoa(r.pageLimit()))
		query.Add("expand[]", "data.default_payment_method")
		if startingAfter != "" {
			query.Set("starting_after", startingAfter)
		}
		body, err := r.get(ctx, "/v1/subscriptions?"+query.Encode())
		if err != nil {
			return nil, fmt.Errorf("list stripe customer subscriptions: %w", err)
		}
		var page struct {
			HasMore bool                            `json:"has_more"`
			Data    []stripeSubscriptionPaymentJSON `json:"data"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("parse stripe customer subscriptions: %w", err)
		}
		for _, sub := range page.Data {
			id := strings.TrimSpace(sub.ID)
			if id == "" {
				continue
			}
			out = append(out, stripeSubscriptionPayment{
				ID:                     id,
				DefaultPaymentMethodID: stripeObjectID(sub.DefaultPaymentMethodRaw),
			})
		}
		if !page.HasMore || len(page.Data) == 0 {
			break
		}
		startingAfter = strings.TrimSpace(page.Data[len(page.Data)-1].ID)
		if startingAfter == "" {
			return nil, errors.New("stripe subscriptions page has_more without a final id")
		}
	}
	return out, nil
}

func parseStripePaymentMethodState(body []byte) (*StripePaymentMethodState, error) {
	var paymentMethod struct {
		ID       string            `json:"id"`
		Customer json.RawMessage   `json:"customer"`
		Card     StripeCardDetails `json:"card"`
	}
	if err := json.Unmarshal(body, &paymentMethod); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(paymentMethod.ID)
	if id == "" {
		return nil, errors.New("stripe payment method response missing id")
	}
	return &StripePaymentMethodState{
		ID:         id,
		CustomerID: stripeObjectID(paymentMethod.Customer),
		Card:       NormalizeStripeCard(paymentMethod.Card),
	}, nil
}

func stripeObjectID(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return strings.TrimSpace(id)
	}
	var object struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return ""
	}
	return strings.TrimSpace(object.ID)
}

func (r *HTTPStripePaymentStateReader) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL()+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(r.SecretKey))
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrStripeObjectNotFound
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("stripe API error (%d)", resp.StatusCode)
	}
	return body, nil
}

func (r *HTTPStripePaymentStateReader) baseURL() string {
	if base := strings.TrimSpace(r.BaseURL); base != "" {
		return strings.TrimRight(base, "/")
	}
	return stripeRESTBaseURL
}

func (r *HTTPStripePaymentStateReader) client() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return stripeapi.ReadOnlyClient(20 * time.Second)
}

func (r *HTTPStripePaymentStateReader) pageLimit() int {
	if r.PageLimit > 0 && r.PageLimit <= 100 {
		return r.PageLimit
	}
	return 100
}
