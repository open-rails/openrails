package hostedcheckout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Store keeps checkouts on their orders.
type Store struct{ db *db.DB }

func NewStore(d *db.DB) *Store { return &Store{db: d} }

// Attach gives an open merchant order its checkout, in the caller's
// transaction (q), and answers the checkout URL on origin. It is refused
// when the order has one already or is not an open merchant order.
func Attach(ctx context.Context, q *gen.Queries, origin string, mid billing.MerchantID, c Checkout, now time.Time) (string, error) {
	if origin == "" {
		return "", ErrUnavailable
	}
	secret, hash, err := NewSecret()
	if err != nil {
		return "", err
	}
	n, err := q.SetOrderCheckout(ctx, gen.SetOrderCheckoutParams{
		SecretHash: hash, ExpiresAt: c.ExpiresAt, SuccessUrl: c.SuccessURL, CancelUrl: c.CancelURL,
		SavedPaymentMethods: c.SavedPaymentMethods, Now: now, MerchantID: mid.UUID(), ID: c.OrderID.UUID(),
	})
	switch {
	case err != nil:
		return "", err
	case n != 1:
		return "", fmt.Errorf("%w: only an open merchant order without a checkout takes one", billing.ErrInvalid)
	}
	return URL(origin, c.OrderID, secret), nil
}

// Resolve finds the checkout a presented secret opens: ErrNotFound for none
// (or one belonging to a closed merchant), ErrExpired past its expiry.
func (s *Store) Resolve(ctx context.Context, secret string, now time.Time) (Checkout, error) {
	hash, ok := Hash(secret)
	if !ok || s == nil || s.db == nil {
		return Checkout{}, ErrNotFound
	}
	mids, err := s.db.GenDirectory().ResolveOrderCheckoutMerchant(ctx, hash)
	if err != nil {
		return Checkout{}, err
	}
	if len(mids) != 1 {
		return Checkout{}, ErrNotFound
	}
	mid := billing.MerchantID(mids[0])
	row, err := s.db.Gen(ctx).GetOrderCheckout(ctx, gen.GetOrderCheckoutParams{MerchantID: mid.UUID(), SecretHash: hash})
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkout{}, ErrNotFound
	}
	if err != nil {
		return Checkout{}, err
	}
	if row.CheckoutExpiresAt == nil || row.CheckoutSuccessUrl == nil || row.CheckoutSavedPaymentMethods == nil {
		return Checkout{}, ErrNotFound
	}
	c := Checkout{
		MerchantID: mid, OrderID: billing.OrderID(row.ID), CustomerID: billing.CustomerID(row.CustomerID),
		ExpiresAt: *row.CheckoutExpiresAt, SuccessURL: *row.CheckoutSuccessUrl, CancelURL: row.CheckoutCancelUrl,
		SavedPaymentMethods: *row.CheckoutSavedPaymentMethods,
	}
	if c.Expired(now) {
		return c, ErrExpired
	}
	return c, nil
}

// OfOrder is the checkout stored on an order row; false without one.
func OfOrder(o gen.BillingOrder, mid billing.MerchantID) (Checkout, bool) {
	if o.CheckoutExpiresAt == nil || o.CheckoutSuccessUrl == nil || o.CheckoutSavedPaymentMethods == nil {
		return Checkout{}, false
	}
	return Checkout{
		MerchantID: mid, OrderID: billing.OrderID(o.ID), CustomerID: billing.CustomerID(o.CustomerID),
		ExpiresAt: *o.CheckoutExpiresAt, SuccessURL: *o.CheckoutSuccessUrl, CancelURL: o.CheckoutCancelUrl,
		SavedPaymentMethods: *o.CheckoutSavedPaymentMethods,
	}, true
}
