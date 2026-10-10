package hostedcheckout

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
)

// Stripe's bounds; the default is the owner's (Adyen's session default).
const (
	DefaultLifetime = time.Hour
	MinLifetime     = 30 * time.Minute
	MaxLifetime     = 24 * time.Hour
)

// OrderIDPlaceholder in a return URL becomes the order's id (Stripe's
// {CHECKOUT_SESSION_ID}).
const OrderIDPlaceholder = "{ORDER_ID}"

const maxReturnURLBytes = 2048

var (
	// ErrNotFound: no live checkout has the secret.
	ErrNotFound = errors.New("hostedcheckout: checkout not found")
	// ErrExpired: the checkout's URL expired.
	ErrExpired = errors.New("hostedcheckout: checkout expired")
	// ErrUnavailable: the deployment serves no checkout host.
	ErrUnavailable = errors.New("hostedcheckout: no checkout host")
)

// Checkout is a hosted checkout as stored on its order.
type Checkout struct {
	MerchantID          billing.MerchantID
	OrderID             billing.OrderID
	CustomerID          billing.CustomerID
	ExpiresAt           time.Time
	SuccessURL          string
	CancelURL           *string
	SavedPaymentMethods bool
}

// Expired reports whether the checkout's URL is past its expiry at now.
func (c Checkout) Expired(now time.Time) bool { return !c.ExpiresAt.After(now) }

// Wire is the checkout as an order shows it; url is set only on the
// response that minted it.
func (c Checkout) Wire(url *string) *billing.OrderCheckout {
	return &billing.OrderCheckout{URL: url, ExpiresAt: c.ExpiresAt.UTC(), SuccessURL: c.SuccessURL, CancelURL: c.CancelURL, SavedPaymentMethods: c.SavedPaymentMethods}
}

// Plan resolves a merchant's checkout request for a new order: its expiry
// (orderExpiry, when the order has its own, bounds it) and return URLs with
// the order id filled in. A request that cannot be honoured is
// billing.ErrInvalid.
func Plan(p billing.OrderCheckoutParams, order billing.OrderID, now time.Time, orderExpiry *time.Time) (Checkout, error) {
	expires := now.Add(DefaultLifetime)
	if p.ExpiresAt != nil {
		expires = p.ExpiresAt.UTC()
	}
	if orderExpiry != nil && p.ExpiresAt == nil && orderExpiry.Before(expires) {
		expires = *orderExpiry
	}
	switch life := expires.Sub(now); {
	case life < MinLifetime || life > MaxLifetime:
		return Checkout{}, fmt.Errorf("%w: checkout.expires_at must be %s to %s away", billing.ErrInvalid, MinLifetime, MaxLifetime)
	case orderExpiry != nil && expires.After(*orderExpiry):
		return Checkout{}, fmt.Errorf("%w: checkout.expires_at is after the order's expires_at", billing.ErrInvalid)
	}
	success, err := returnURL("checkout.success_url", p.SuccessURL, order)
	if err != nil {
		return Checkout{}, err
	}
	c := Checkout{OrderID: order, ExpiresAt: expires, SuccessURL: success, SavedPaymentMethods: p.SavedPaymentMethods}
	if p.CancelURL != nil {
		cancel, err := returnURL("checkout.cancel_url", *p.CancelURL, order)
		if err != nil {
			return Checkout{}, err
		}
		c.CancelURL = &cancel
	}
	return c, nil
}

// returnURL checks one return URL and fills in the order id. Any site may
// be named (Stripe): only the merchant's own backend sets it. It is https,
// or http on a loopback host.
func returnURL(field, raw string, order billing.OrderID) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxReturnURLBytes {
		return "", fmt.Errorf("%w: %s is required, at most %d bytes", billing.ErrInvalid, field, maxReturnURLBytes)
	}
	filled := strings.ReplaceAll(raw, OrderIDPlaceholder, order.String())
	u, err := url.Parse(filled)
	if err != nil || u.User != nil || u.Opaque != "" || u.Host == "" || !(u.Scheme == "https" || u.Scheme == "http" && loopback(u.Hostname())) {
		return "", fmt.Errorf("%w: %s must be an absolute https URL (http on loopback) without credentials", billing.ErrInvalid, field)
	}
	return filled, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SavedCards is what the checkout page shows of a customer's saved cards:
// active ones the customer allowed to be shown again (Stripe's
// allow_redisplay), as the card and nothing about what it pays.
func SavedCards(methods []billing.PaymentMethod) []billing.PaymentMethod {
	out := make([]billing.PaymentMethod, 0, len(methods))
	for _, m := range methods {
		if m.Status != billing.PaymentMethodActive || !m.Reusable {
			continue
		}
		out = append(out, billing.PaymentMethod{
			ID: m.ID, CustomerID: m.CustomerID, Rail: m.Rail, PSPID: m.PSPID, Status: m.Status, Card: m.Card,
			Health: m.Health, Reusable: true, Mandates: []billing.Mandate{}, Subscriptions: []billing.PaymentMethodSubscription{},
			DefaultCurrencies: []string{}, CreatedAt: m.CreatedAt,
		})
	}
	return out
}
