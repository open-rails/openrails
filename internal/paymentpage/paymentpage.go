package paymentpage

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/apperr"
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
	// ErrNotFound: no live pay URL has the secret.
	ErrNotFound = errors.New("paymentpage: pay URL not found")
	// ErrExpired: the pay URL expired.
	ErrExpired = errors.New("paymentpage: pay URL expired")
)

// PayURL is an order's pay URL as stored on it: the merchant's, or the
// customer's own.
type PayURL struct {
	MerchantID billing.MerchantID
	OrderID    billing.OrderID
	CustomerID billing.CustomerID
	Embedded   bool
	ExpiresAt  time.Time
	// SuccessURL is a merchant order's return; nil on a customer's.
	SuccessURL          *string
	CancelURL           *string
	SavedPaymentMethods bool
}

// Expired reports whether the pay URL is past its expiry at now.
func (p PayURL) Expired(now time.Time) bool { return !p.ExpiresAt.After(now) }

// Show puts the pay URL on its order; url is the URL itself, known only to
// the answer that made it or a re-ask.
func (p PayURL) Show(o *billing.Order, url *string) {
	expires := p.ExpiresAt.UTC()
	o.PayURL, o.PayURLExpiresAt, o.Embedded = url, &expires, p.Embedded
	o.SuccessURL, o.CancelURL, o.SavedPaymentMethods = p.SuccessURL, p.CancelURL, p.SavedPaymentMethods
}

// Plan resolves a merchant's pay URL request for a new order: its expiry
// (orderExpiry, when the order has its own, bounds it) and return URLs with
// the order id filled in. A request that cannot be honoured is invalid_param
// naming its field.
func Plan(p billing.OrderPayURLParams, order billing.OrderID, now time.Time, orderExpiry *time.Time) (PayURL, error) {
	expires := now.Add(DefaultLifetime)
	if p.ExpiresAt != nil {
		expires = p.ExpiresAt.UTC()
	}
	if orderExpiry != nil && p.ExpiresAt == nil && orderExpiry.Before(expires) {
		expires = *orderExpiry
	}
	switch life := expires.Sub(now); {
	case life < MinLifetime || life > MaxLifetime:
		return PayURL{}, apperr.Invalidf("pay_url.expires_at must be %s to %s away", MinLifetime, MaxLifetime).WithParam("pay_url.expires_at")
	case orderExpiry != nil && expires.After(*orderExpiry):
		return PayURL{}, apperr.Invalidf("pay_url.expires_at is after the order's expires_at").WithParam("pay_url.expires_at")
	}
	success, err := returnURL("pay_url.success_url", p.SuccessURL, order)
	if err != nil {
		return PayURL{}, err
	}
	out := PayURL{OrderID: order, Embedded: p.Embedded, ExpiresAt: expires, SuccessURL: &success, SavedPaymentMethods: p.SavedPaymentMethods}
	if p.CancelURL != nil {
		cancel, err := returnURL("pay_url.cancel_url", *p.CancelURL, order)
		if err != nil {
			return PayURL{}, err
		}
		out.CancelURL = &cancel
	}
	return out, nil
}

// returnURL checks one return URL and fills in the order id. Any site may
// be named (Stripe): only the merchant's own backend sets it. It is https,
// or http on a loopback host.
func returnURL(field, raw string, order billing.OrderID) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxReturnURLBytes {
		return "", apperr.Invalidf("%s is required, at most %d bytes", field, maxReturnURLBytes).WithParam(field)
	}
	filled := strings.ReplaceAll(raw, OrderIDPlaceholder, order.String())
	u, err := url.Parse(filled)
	if err != nil || u.User != nil || u.Opaque != "" || u.Host == "" || !(u.Scheme == "https" || u.Scheme == "http" && loopback(u.Hostname())) {
		return "", apperr.Invalidf("%s must be an absolute https URL (http on loopback) without credentials", field).WithParam(field)
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

// SavedCards is what the payment page shows of a customer's saved cards:
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
